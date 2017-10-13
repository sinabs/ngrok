package server

import (
	"fmt"
	"io"
	"ngrok/conn"
	"ngrok/msg"
	"ngrok/util"
	"ngrok/version"
	"runtime/debug"
	"strings"
	"time"
	"bytes"
	"net/http"
	"os"
	"io/ioutil"
)

const (
	pingTimeoutInterval = 30 * time.Second
	connReapInterval    = 10 * time.Second
	controlWriteTimeout = 10 * time.Second
	proxyStaleDuration  = 60 * time.Second
	proxyMaxPoolSize    = 10
)

type Control struct {
	// auth message
	auth *msg.Auth

	// actual connection
	conn conn.Conn

	// put a message in this channel to send it over
	// conn to the client
	out chan (msg.Message)

	// read from this channel to get the next message sent
	// to us over conn by the client
	in chan (msg.Message)

	// the last time we received a ping from the client - for heartbeats
	lastPing time.Time

	// all of the tunnels this control connection handles
	tunnels []*Tunnel

	// proxy connections
	proxies chan conn.Conn

	// identifier
	id string

	// synchronizer for controlled shutdown of writer()
	writerShutdown *util.Shutdown

	// synchronizer for controlled shutdown of reader()
	readerShutdown *util.Shutdown

	// synchronizer for controlled shutdown of manager()
	managerShutdown *util.Shutdown

	// synchronizer for controller shutdown of entire Control
	shutdown *util.Shutdown
}

func NewControl(ctlConn conn.Conn, authMsg *msg.Auth) {
	var err error

	// create the object
	c := &Control{
		auth:            authMsg,
		conn:            ctlConn,
		out:             make(chan msg.Message),
		in:              make(chan msg.Message),
		proxies:         make(chan conn.Conn, 10),
		lastPing:        time.Now(),
		writerShutdown:  util.NewShutdown(),
		readerShutdown:  util.NewShutdown(),
		managerShutdown: util.NewShutdown(),
		shutdown:        util.NewShutdown(),
	}

	failAuth := func(e error) {
		_ = msg.WriteMsg(ctlConn, &msg.AuthResp{Error: e.Error()})
		ctlConn.Close()
	}

	// register the clientid
	c.id = authMsg.ClientId
	if c.id == "" {
		// it's a new session, assign an ID
		if c.id, err = util.SecureRandId(16); err != nil {
			failAuth(err)
			return
		}
	}

	// set logging prefix
	ctlConn.SetType("ctl")
	ctlConn.AddLogPrefix(c.id)

	if authMsg.Version != version.Proto {
		failAuth(fmt.Errorf("Incompatible versions. Server %s, client %s. Download a new version at http://ngrok.com", version.MajorMinor(), authMsg.Version))
		return
	}

	// register the control
	if replaced := controlRegistry.Add(c.id, c); replaced != nil {
		replaced.shutdown.WaitComplete()
	}

	// start the writer first so that the following messages get sent
	go c.writer()

	// Respond to authentication
	c.out <- &msg.AuthResp{
		Version:   version.Proto,
		MmVersion: version.MajorMinor(),
		ClientId:  c.id,
	}

	// As a performance optimization, ask for a proxy connection up front
	c.out <- &msg.ReqProxy{}

	// manage the connection
	go c.manager()
	go c.reader()
	go c.stopper()
}

// Register a new tunnel on this control connection
func (c *Control) registerTunnel(rawTunnelReq *msg.ReqTunnel) {
	for _, proto := range strings.Split(rawTunnelReq.Protocol, "+") {
		tunnelReq := *rawTunnelReq
		tunnelReq.Protocol = proto

		c.conn.Debug("注册新隧道")
		t, err := NewTunnel(&tunnelReq, c)
		if err != nil {
			c.out <- &msg.NewTunnel{Error: err.Error()}
			if len(c.tunnels) == 0 {
				c.shutdown.Begin()
			}

			// we're done
			return
		}

		// add it to the list of tunnels
		c.tunnels = append(c.tunnels, t)

		// acknowledge success
		c.out <- &msg.NewTunnel{
			Url:      t.url,
			Protocol: proto,
			ReqId:    rawTunnelReq.ReqId,
		}

		rawTunnelReq.Hostname = strings.Replace(t.url, proto+"://", "", 1)
	}
}

func (c *Control) manager() {
	// don't crash on panics
	defer func() {
		if err := recover(); err != nil {
			c.conn.Info("Control::manager failed with error %v: %s", err, debug.Stack())
		}
	}()

	// kill everything if the control manager stops
	defer c.shutdown.Begin()

	// notify that manager() has shutdown
	defer c.managerShutdown.Complete()

	// reaping timer for detecting heartbeat failure
	reap := time.NewTicker(connReapInterval)
	defer reap.Stop()

	for {
		select {
		case <-reap.C:
			if time.Since(c.lastPing) > pingTimeoutInterval {
				c.conn.Info("Lost heartbeat")
				c.shutdown.Begin()
			}

		case mRaw, ok := <-c.in:
			// c.in closes to indicate shutdown
			if !ok {
				return
			}

			switch m := mRaw.(type) {
			case *msg.ReqTunnel:
				c.registerTunnel(m)

			case *msg.Ping:
				c.lastPing = time.Now()
				c.out <- &msg.Pong{}
			}
		}
	}
}

func (c *Control) writer() {
	defer func() {
		if err := recover(); err != nil {
			c.conn.Info("Control::writer failed with error %v: %s", err, debug.Stack())
		}
	}()

	// kill everything if the writer() stops
	defer c.shutdown.Begin()

	// notify that we've flushed all messages
	defer c.writerShutdown.Complete()

	// write messages to the control channel
	for m := range c.out {
		c.conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
		if err := msg.WriteMsg(c.conn, m); err != nil {
			panic(err)
		}
	}
}

func (c *Control) reader() {
	defer func() {
		if err := recover(); err != nil {
			c.conn.Warn("Control::reader failed with error %v: %s", err, debug.Stack())
		}
	}()

	// kill everything if the reader stops
	defer c.shutdown.Begin()

	// notify that we're done
	defer c.readerShutdown.Complete()

	// read messages from the control channel
	for {
		if msg, err := msg.ReadMsg(c.conn); err != nil {
			if err == io.EOF {
				c.conn.Info("EOF")
				return
			} else {
				panic(err)
			}
		} else {
			// this can also panic during shutdown
			c.in <- msg
		}
	}
}

func (c *Control) stopper() {
	defer func() {
		if r := recover(); r != nil {
			c.conn.Error("无法关闭控制: %v", r)
		}
	}()

	// wait until we're instructed to shutdown
	c.shutdown.WaitBegin()

	// remove ourself from the control registry
	controlRegistry.Del(c.id)

	// shutdown manager() so that we have no more work to do
	close(c.in)
	c.managerShutdown.WaitComplete()

	// shutdown writer()
	close(c.out)
	c.writerShutdown.WaitComplete()

	// close connection fully
	c.conn.Close()

	// shutdown all of the tunnels
	for _, t := range c.tunnels {
		t.Shutdown()
	}

	// shutdown all of the proxy connections
	close(c.proxies)
	for p := range c.proxies {
		p.Close()
	}

	c.shutdown.Complete()
	c.conn.Info("关闭完成")
}

func (c *Control) RegisterProxy(conn conn.Conn) {
	conn.AddLogPrefix(c.id)

	conn.SetDeadline(time.Now().Add(proxyStaleDuration))
	select {
	case c.proxies <- conn:
		conn.Info("已注册")
	default:
		conn.Info("代理缓冲区已满，丢弃。")
		conn.Close()
	}
}

// Remove a proxy connection from the pool and return it
// If not proxy connections are in the pool, request one
// and wait until it is available
// Returns an error if we couldn't get a proxy because it took too long
// or the tunnel is closing
func (c *Control) GetProxy() (proxyConn conn.Conn, err error) {
	var ok bool

	// get a proxy connection from the pool
	select {
	case proxyConn, ok = <-c.proxies:
		if !ok {
			err = fmt.Errorf("没有可用的代理连接，控制正在关闭")
			return
		}
	default:
		// no proxy available in the pool, ask for one over the control channel
		c.conn.Debug("池中没有代理，正在请求代理控制 . . .")
		if err = util.PanicToError(func() { c.out <- &msg.ReqProxy{} }); err != nil {
			return
		}

		select {
		case proxyConn, ok = <-c.proxies:
			if !ok {
				err = fmt.Errorf("没有可用的代理连接，控制正在关闭")
				return
			}

		case <-time.After(pingTimeoutInterval):
			err = fmt.Errorf("尝试获取代理连接时超时")
			return
		}
	}
	return
}

// Called when this control is replaced by another control
// this can happen if the network drops out and the client reconnects
// before the old tunnel has lost its heartbeat
func (c *Control) Replaced(replacement *Control) {
	c.conn.Info("替换为控制: %s", replacement.conn.Id())

	// set the control id to empty string so that when stopper()
	// calls registry.Del it won't delete the replacement
	c.id = ""

	// tell the old one to shutdown
	c.shutdown.Begin()
}

func (c *Control) TmsRequest(method, eventsType string, body *bytes.Reader) (resp *http.Response, err error) {

	apiUrl := os.Getenv("API_URL")
	if apiUrl == "" {
		return
	}

	path := fmt.Sprintf("https://%s/api/ngrok/events/%s", apiUrl, eventsType)
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return
	}

	if body != nil {
		req.Header.Add("Content-Type", "application/json")
		req.ContentLength = int64(body.Len())
	}

	requestStartAt := time.Now()
	client := &http.Client{}
	resp, err = client.Do(req)

	if err != nil {
		c.conn.Error("无法将指标事件发送到 TMS %v", err)
	} else {
		c.conn.Info("TMS 处理的请求 %f 秒", time.Since(requestStartAt).Seconds())
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			bytes, _ := ioutil.ReadAll(resp.Body)
			c.conn.Error("获得 %v 响应， 从 TMS: %s", resp.StatusCode, bytes)
		}
	}

	return
}