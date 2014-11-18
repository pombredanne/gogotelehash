package bridge

import (
	"sync"

	"github.com/telehash/gogotelehash/e3x"
	"github.com/telehash/gogotelehash/e3x/cipherset"
	"github.com/telehash/gogotelehash/transports"
	"github.com/telehash/gogotelehash/util/logs"
)

type Bridge interface {
	RouteToken(token cipherset.Token, source, target *e3x.Exchange)
	BreakRoute(token cipherset.Token)
}

type module struct {
	mtx             sync.RWMutex
	e               *e3x.Endpoint
	packetRoutes    map[cipherset.Token]*e3x.Exchange
	handshakeRoutes map[cipherset.Token]*e3x.Exchange
	log             *logs.Logger
}

type moduleKeyType string

const moduleKey = moduleKeyType("bridge")

func Module() func(*e3x.Endpoint) error {
	return func(e *e3x.Endpoint) error {
		return e3x.RegisterModule(moduleKey, newBridge(e))(e)
	}
}

func FromEndpoint(e *e3x.Endpoint) Bridge {
	mod := e.Module(moduleKey)
	if mod == nil {
		return nil
	}
	return mod.(*module)
}

func newBridge(e *e3x.Endpoint) *module {
	return &module{
		e:               e,
		packetRoutes:    make(map[cipherset.Token]*e3x.Exchange),
		handshakeRoutes: make(map[cipherset.Token]*e3x.Exchange),
	}
}

func (mod *module) Init() error {
	mod.log = logs.Module("bridge").From(mod.e.LocalHashname())

	e3x.TransportsFromEndpoint(mod.e).Wrap(func(c transports.Config) transports.Config {
		return transportConfig{mod, c}
	})

	e3x.ObserversFromEndpoint(mod.e).Register(mod.on_exchange_closed)

	return nil
}

func (mod *module) Start() error { return nil }
func (mod *module) Stop() error  { return nil }

func (mod *module) RouteToken(token cipherset.Token, source, target *e3x.Exchange) {
	mod.mtx.Lock()
	mod.packetRoutes[token] = source
	if target != nil {
		mod.handshakeRoutes[token] = target
	}
	mod.mtx.Unlock()
}

func (mod *module) BreakRoute(token cipherset.Token) {
	mod.mtx.Lock()
	delete(mod.packetRoutes, token)
	delete(mod.handshakeRoutes, token)
	mod.mtx.Unlock()
}

func (mod *module) lookupToken(token cipherset.Token) (source, target *e3x.Exchange) {
	mod.mtx.RLock()
	source = mod.packetRoutes[token]
	target = mod.handshakeRoutes[token]
	mod.mtx.RUnlock()
	return
}

func (mod *module) on_exchange_closed(e *e3x.ExchangeClosedEvent) {
	mod.mtx.Lock()
	defer mod.mtx.Unlock()

	for token, x := range mod.packetRoutes {
		if e.Exchange == x {
			delete(mod.packetRoutes, token)
			delete(mod.handshakeRoutes, token)
		}
	}

	for token, x := range mod.handshakeRoutes {
		if e.Exchange == x {
			delete(mod.packetRoutes, token)
			delete(mod.handshakeRoutes, token)
		}
	}
}

type transportConfig struct {
	mod *module
	c   transports.Config
}

func (c transportConfig) Open() (transports.Transport, error) {
	t, err := c.c.Open()
	if err != nil {
		return nil, err
	}
	return &transport{c.mod, t}, nil
}

type transport struct {
	mod *module
	t   transports.Transport
}

func (t *transport) LocalAddresses() []transports.Addr {
	return t.t.LocalAddresses()
}

func (t *transport) ReadMessage(p []byte) (n int, src transports.Addr, err error) {
	for {
		n, src, err = t.t.ReadMessage(p)
		if err != nil {
			return n, src, err
		}

		buf := p[:n]

		var (
			token          = cipherset.ExtractToken(buf)
			source, target = t.mod.lookupToken(token)
		)

		// not a bridged message
		if source == nil {
			return n, src, err
		}

		// detect message type
		var (
			msgtype = "PKT"
			ex      = source
		)
		if buf[0] == 0 && buf[1] == 1 {
			msgtype = "HDR"
			ex = target
		}

		// handle bridged message
		err = t.t.WriteMessage(buf, ex.ActivePath())
		if err != nil {
			// TODO handle error
			t.mod.log.To(ex.RemoteHashname()).Printf("\x1B[35mFWD %s %x %s error=%s\x1B[0m", msgtype, token, ex.ActivePath(), err)
		} else {
			t.mod.log.To(ex.RemoteHashname()).Printf("\x1B[35mFWD %s %x %s\x1B[0m", msgtype, token, ex.ActivePath())
		}

		// continue reading messages
	}
}

func (t *transport) WriteMessage(p []byte, dst transports.Addr) error {
	return t.t.WriteMessage(p, dst)
}

func (t *transport) Close() error {
	return t.t.Close()
}
