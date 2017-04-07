package chserver

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	socks5 "github.com/armon/go-socks5"
	"github.com/gorilla/websocket"
	"github.com/jpillora/chisel/share"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	KeySeed  string
	AuthFile string
	Auth     string
	Proxy    string
	Socks5   bool
}

type Server struct {
	*chshare.Logger
	//Users is an empty map of usernames to Users
	//It can be optionally initialized using the
	//file found at AuthFile
	Users    chshare.Users
	sessions chshare.Users

	fingerprint  string
	wsCount      int
	httpServer   *chshare.HTTPServer
	reverseProxy *httputil.ReverseProxy
	sshConfig    *ssh.ServerConfig
	socksServer  *socks5.Server
}

func NewServer(config *Config) (*Server, error) {
	s := &Server{
		Logger:     chshare.NewLogger("server"),
		httpServer: chshare.NewHTTPServer(),
		sessions:   chshare.Users{},
	}
	s.Info = true

	//parse users, if provided
	if config.AuthFile != "" {
		users, err := chshare.ParseUsers(config.AuthFile)
		if err != nil {
			return nil, err
		}
		s.Users = users
	}
	//parse single user, if provided
	if config.Auth != "" {
		u := &chshare.User{Addrs: []*regexp.Regexp{chshare.UserAllowAll}}
		u.Name, u.Pass = chshare.ParseAuth(config.Auth)
		if u.Name != "" {
			if s.Users == nil {
				s.Users = chshare.Users{}
			}
			s.Users[u.Name] = u
		}
	}

	//generate private key (optionally using seed)
	key, _ := chshare.GenerateKey(config.KeySeed)
	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal("Failed to parse key")
	}
	//fingerprint this key
	s.fingerprint = chshare.FingerprintKey(private.PublicKey())
	//create ssh config
	s.sshConfig = &ssh.ServerConfig{
		ServerVersion:    chshare.ProtocolVersion + "-server",
		PasswordCallback: s.authUser,
	}
	s.sshConfig.AddHostKey(private)
	//setup reverse proxy
	if config.Proxy != "" {
		u, err := url.Parse(config.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, s.Errorf("Missing protocol (%s)", u)
		}
		s.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		//always use proxy host
		s.reverseProxy.Director = func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}
	//setup socks server (not listening on any port!)
	if config.Socks5 {
		socksConfig := &socks5.Config{}
		if s.Debug {
			socksConfig.Logger = log.New(os.Stdout, "[socks]", log.Ldate|log.Ltime)
		} else {
			socksConfig.Logger = log.New(ioutil.Discard, "", 0)
		}
		s.socksServer, err = socks5.New(socksConfig)
		if err != nil {
			return nil, err
		}
		s.Infof("SOCKS5 Enabled")
	}
	//ready!
	return s, nil
}

func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}
	return s.Wait()
}

func (s *Server) Start(host, port string) error {
	s.Infof("Fingerprint %s", s.fingerprint)
	if len(s.Users) > 0 {
		s.Infof("User authenication enabled")
	}
	if s.reverseProxy != nil {
		s.Infof("Reverse proxy enabled")
	}
	s.Infof("Listening on %s...", port)

	h := http.Handler(http.HandlerFunc(s.handleHTTP))
	if s.Debug {
		h = requestlog.Wrap(h)
	}
	return s.httpServer.GoListenAndServe(host+":"+port, h)
}

func (s *Server) Wait() error {
	return s.httpServer.Wait()
}

func (s *Server) Close() error {
	//this should cause an error in the open websockets
	return s.httpServer.Close()
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	protocol := r.Header.Get("Sec-WebSocket-Protocol")
	//websockets upgrade AND has chisel prefix
	if upgrade == "websocket" && protocol == chshare.ProtocolVersion {
		s.handleWS(w, r)
		return
	}
	//proxy target was provided
	if s.reverseProxy != nil {
		s.reverseProxy.ServeHTTP(w, r)
		return
	}
	//missing :O
	w.WriteHeader(404)
	w.Write([]byte("Not found"))
}

//
func (s *Server) authUser(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	// no auth - allow all
	if len(s.Users) == 0 {
		return nil, nil
	}
	// authenticate user
	n := c.User()
	u, ok := s.Users[n]
	if !ok || u.Pass != string(pass) {
		s.Debugf("Login failed: %s", n)
		return nil, errors.New("Invalid auth")
	}
	//insert session
	s.sessions[string(c.SessionID())] = u
	return nil, nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func (s *Server) handleWS(w http.ResponseWriter, req *http.Request) {
	wsConn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		s.Debugf("Failed to upgrade (%s)", err)
		return
	}
	conn := chshare.NewWebSocketConn(wsConn)
	// perform SSH handshake on net.Conn
	s.Debugf("Handshaking...")
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshConfig)
	if err != nil {
		s.Debugf("Failed to handshake (%s)", err)
		return
	}
	//load user
	var user *chshare.User
	if len(s.Users) > 0 {
		sid := string(sshConn.SessionID())
		user = s.sessions[sid]
		defer delete(s.sessions, sid)
	}

	//verify configuration
	s.Debugf("Verifying configuration")

	//wait for request, with timeout
	var r *ssh.Request
	select {
	case r = <-reqs:
	case <-time.After(10 * time.Second):
		sshConn.Close()
		return
	}

	failed := func(err error) {
		r.Reply(false, []byte(err.Error()))
	}
	if r.Type != "config" {
		failed(s.Errorf("expecting config request"))
		return
	}
	c, err := chshare.DecodeConfig(r.Payload)
	if err != nil {
		failed(s.Errorf("invalid config"))
		return
	}
	if c.Version != chshare.BuildVersion {
		v := c.Version
		if v == "" {
			v = "<unknown>"
		}
		s.Infof("Client version (%s) differs from server version (%s)",
			v, chshare.BuildVersion)
	}
	//if user is provided, ensure they have
	//access to the desired remotes
	if user != nil {
		for _, r := range c.Remotes {
			addr := r.RemoteHost + ":" + r.RemotePort
			if !user.HasAccess(addr) {
				failed(s.Errorf("access to '%s' denied", addr))
				return
			}
		}
	}
	//success!
	r.Reply(true, nil)

	//prepare connection logger
	s.wsCount++
	id := s.wsCount
	l := s.Fork("session#%d", id)
	l.Debugf("Open")
	go s.handleSSHRequests(l, reqs)
	go s.handleSSHChannels(l, chans)
	sshConn.Wait()
	l.Debugf("Close")
}

func (s *Server) handleSSHRequests(l *chshare.Logger, reqs <-chan *ssh.Request) {
	for r := range reqs {
		switch r.Type {
		case "ping":
			r.Reply(true, nil)
		default:
			l.Debugf("Unknown request: %s", r.Type)
		}
	}
}

func (s *Server) handleSSHChannels(l *chshare.Logger, chans <-chan ssh.NewChannel) {
	var connCount int32
	for ch := range chans {
		remote := string(ch.ExtraData())
		socks := remote == "socks"
		//dont accept socks when --socks5 isn't enabled
		if socks && s.socksServer == nil {
			l.Debugf("Denied socks request, please enable --socks5")
			ch.Reject(ssh.Prohibited, "SOCKS5 is not enabled on the server")
			continue
		}
		//accept rest
		stream, reqs, err := ch.Accept()
		if err != nil {
			l.Debugf("Failed to accept stream: %s", err)
			continue
		}
		go ssh.DiscardRequests(reqs)
		//handle stream type
		connID := atomic.AddInt32(&connCount, 1)
		if socks {
			go s.handleSocksStream(l.Fork("socks#%d", connID), stream)
		} else {
			go s.handleTCPStream(l.Fork("tcp#%d", connID), stream, remote)
		}
	}
}

func (s *Server) handleSocksStream(l *chshare.Logger, src io.ReadWriteCloser) {
	l.Debugf("Openning")
	conn := chshare.NewRWCConn(src)
	if err := s.socksServer.ServeConn(conn); err != nil {
		l.Debugf("socks error: %s", err)
		src.Close()
		return
	}
	l.Debugf("Closed")
}

func (s *Server) handleTCPStream(l *chshare.Logger, src io.ReadWriteCloser, remote string) {
	dst, err := net.Dial("tcp", remote)
	if err != nil {
		l.Debugf("remote: %s (%s)", remote, err)
		src.Close()
		return
	}
	l.Debugf("Open")
	sent, received := chshare.Pipe(src, dst)
	l.Debugf("Close (sent %d received %d)", sent, received)
}
