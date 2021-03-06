package chord

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"

	"github.com/armon/go-chord"

	"github.com/telehash/gogotelehash/e3x"
	"github.com/telehash/gogotelehash/internal/hashname"
	"github.com/telehash/gogotelehash/internal/lob"
	"github.com/telehash/gogotelehash/modules/mesh"
)

var _ chord.Transport = (*transport)(nil)

type moduleKey string

type Ring interface {
	Create() error
	Join(existing *e3x.Addr) error
	Lookup(n int, key []byte) ([]*chord.Vnode, error)
}

type ring struct {
	endpoint *e3x.Endpoint
	conf     *chord.Config
	ring     *chord.Ring
}

func Register(e *e3x.Endpoint, key string, conf *chord.Config) {
	e.Use(moduleKey(key), &ring{e, conf, nil})
}

func FromEndpoint(e *e3x.Endpoint, key string) Ring {
	mod := e.Module(moduleKey(key))
	if mod == nil {
		return nil
	}
	return mod.(*ring)
}

func DefaultConfig(hn hashname.H) *chord.Config {
	return chord.DefaultConfig(string(hn))
}

func (r *ring) Init() error {
	return nil
}

func (r *ring) Start() error {
	return nil
}

func (r *ring) Stop() error {
	if r.ring == nil {
		return nil
	}

	defer r.ring.Shutdown()
	return r.ring.Leave()
}

func (r *ring) Create() error {
	m := mesh.FromEndpoint(r.endpoint)
	if m == nil {
		panic("Chord requires the `mesh` module")
	}

	ring, err := chord.Create(r.conf, newTransport(r.endpoint, m))
	if err != nil {
		return err
	}

	r.ring = ring
	return nil
}

func (r *ring) Join(existing *e3x.Addr) error {
	m := mesh.FromEndpoint(r.endpoint)
	if m == nil {
		panic("Chord requires the `mesh` module")
	}

	t := newTransport(r.endpoint, m)
	t.registerAddr(existing)
	ring, err := chord.Join(r.conf, t, string(existing.Hashname()))
	if err != nil {
		return err
	}

	r.ring = ring
	return nil
}

func (r *ring) Lookup(n int, key []byte) ([]*chord.Vnode, error) {
	return r.ring.Lookup(n, key)
}

type transport struct {
	mtx          sync.Mutex
	e            *e3x.Endpoint
	m            mesh.Mesh
	addressTable map[hashname.H]*e3x.Addr
	localVnodes  map[string]localRPC
}

type localRPC struct {
	vn  *chord.Vnode
	rpc chord.VnodeRPC
}

type completeVnode struct {
	Id   string    `json:"id"`
	Addr *e3x.Addr `json:"addr"`
}

func newTransport(e *e3x.Endpoint, m mesh.Mesh) *transport {
	t := &transport{
		e:            e,
		m:            m,
		addressTable: map[hashname.H]*e3x.Addr{},
		localVnodes:  map[string]localRPC{},
	}

	if addr, _ := e.LocalAddr(); addr != nil {
		t.registerAddr(addr)
	}

	e.AddHandler("chord.list", e3x.HandlerFunc(t.handleListVnodes))
	e.AddHandler("chord.ping", e3x.HandlerFunc(t.handlePing))
	e.AddHandler("chord.predecessor.get", e3x.HandlerFunc(t.handleGetPredecessor))
	e.AddHandler("chord.notify", e3x.HandlerFunc(t.handleNotify))
	e.AddHandler("chord.successors.find", e3x.HandlerFunc(t.handleFindSuccessors))
	e.AddHandler("chord.predecessor.clear", e3x.HandlerFunc(t.handleClearPredecessor))
	e.AddHandler("chord.successor.skip", e3x.HandlerFunc(t.handleSkipSuccessor))

	return t
}

func (t *transport) completeVnode(vn *chord.Vnode) *completeVnode {
	if vn == nil {
		return nil
	}

	id := hex.EncodeToString(vn.Id)

	t.mtx.Lock()
	defer t.mtx.Unlock()

	c := &completeVnode{id, t.addressTable[hashname.H(vn.Host)]}
	return c
}

func (t *transport) internalVnode(c *completeVnode) *chord.Vnode {
	if c == nil {
		return nil
	}

	t.mtx.Lock()
	defer t.mtx.Unlock()

	id, err := hex.DecodeString(c.Id)
	if err != nil {
		return nil
	}

	t.addressTable[c.Addr.Hashname()] = c.Addr
	return &chord.Vnode{id, string(c.Addr.Hashname())}
}

func (t *transport) completeVnodes(vn []*chord.Vnode) []*completeVnode {
	if len(vn) == 0 {
		return nil
	}

	t.mtx.Lock()
	defer t.mtx.Unlock()

	c := make([]*completeVnode, len(vn))
	for i, a := range vn {
		if a != nil {
			b := &completeVnode{hex.EncodeToString(a.Id), t.addressTable[hashname.H(a.Host)]}
			c[i] = b
		}
	}

	return c
}

func (t *transport) internalVnodes(c []*completeVnode) []*chord.Vnode {
	if len(c) == 0 {
		return nil
	}

	t.mtx.Lock()
	defer t.mtx.Unlock()

	vn := make([]*chord.Vnode, len(c))
	for i, a := range c {
		if a != nil {
			id, err := hex.DecodeString(a.Id)
			if err != nil {
				return nil
			}
			t.addressTable[a.Addr.Hashname()] = a.Addr
			b := &chord.Vnode{id, string(a.Addr.Hashname())}
			vn[i] = b
		}
	}

	return vn
}

func (t *transport) lookupAddr(hn hashname.H) *e3x.Addr {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	return t.addressTable[hn]
}

func (t *transport) registerAddr(addr *e3x.Addr) {
	if addr == nil {
		return
	}

	t.mtx.Lock()
	defer t.mtx.Unlock()
	t.addressTable[addr.Hashname()] = addr
}

func (t *transport) lookupRPC(id string) chord.VnodeRPC {
	t.mtx.Lock()
	defer t.mtx.Unlock()
	return t.localVnodes[id].rpc
}

// Gets a list of the vnodes on the box
func (t *transport) ListVnodes(hn string) ([]*chord.Vnode, error) {
	var (
		addr *e3x.Addr
		ch   *e3x.Channel
		res  []*completeVnode
		err  error
	)

	addr = t.lookupAddr(hashname.H(hn))
	if addr == nil {
		return nil, e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.list", true)
	if err != nil {
		return nil, err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	err = ch.WritePacket(&lob.Packet{})
	if err != nil {
		return nil, err
	}

	err = json.NewDecoder(newStream(ch)).Decode(&res)
	if err != nil {
		return nil, err
	}

	return t.internalVnodes(res), nil
}

func (t *transport) handleListVnodes(ch *e3x.Channel) {
	var (
		err error
		res []*completeVnode
	)

	defer ch.Close()

	_, err = ch.ReadPacket()
	if err != nil {
		// log error
		// tracef("error: %s", err)
		return
	}

	for _, rpc := range t.localVnodes {
		res = append(res, t.completeVnode(rpc.vn))
	}

	err = json.NewEncoder(newStream(ch)).Encode(&res)
	if err != nil {
		// log error
		// tracef("error: %s", err)
		return
	}

	// tracef("handle.ListVnodes() => %s", res)
}

// Ping a Vnode, check for liveness
func (t *transport) Ping(vn *chord.Vnode) (bool, error) {
	var (
		addr  *e3x.Addr
		ch    *e3x.Channel
		pkt   *lob.Packet
		alive bool
		err   error
	)

	addr = t.lookupAddr(hashname.H(vn.Host))
	if addr == nil {
		return false, e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.ping", true)
	if err != nil {
		return false, err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	pkt = &lob.Packet{}
	pkt.Header().SetString("vn", vn.String())
	err = ch.WritePacket(pkt)
	if err != nil {
		return false, err
	}

	pkt, err = ch.ReadPacket()
	if err != nil {
		return false, err
	}
	alive, _ = pkt.Header().GetBool("alive")
	// tracef("Ping(Vnode(%q)) => %v", vn.String(), alive)
	return alive, nil
}

func (t *transport) handlePing(ch *e3x.Channel) {
	var (
		err   error
		pkt   *lob.Packet
		id    string
		alive bool
	)

	defer ch.Close()

	pkt, err = ch.ReadPacket()
	if err != nil {
		// log error
		// tracef("error: %s", err)
		return
	}

	id, _ = pkt.Header().GetString("vn")
	rpc := t.lookupRPC(id)
	if rpc == nil {
		alive = false
	} else {
		alive = true
	}

	pkt = &lob.Packet{}
	pkt.Header().SetBool("alive", alive)
	err = ch.WritePacket(pkt)
	if err != nil {
		// log error
		// tracef("error: %s", err)
		return
	}
}

// Request a nodes predecessor
func (t *transport) GetPredecessor(vn *chord.Vnode) (*chord.Vnode, error) {
	var (
		addr *e3x.Addr
		ch   *e3x.Channel
		pkt  *lob.Packet
		res  *completeVnode
		err  error
	)

	addr = t.lookupAddr(hashname.H(vn.Host))
	if addr == nil {
		return nil, e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.predecessor.get", true)
	if err != nil {
		return nil, err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	pkt = &lob.Packet{}
	pkt.Header().SetString("vn", vn.String())
	err = ch.WritePacket(pkt)
	if err != nil {
		return nil, err
	}

	err = json.NewDecoder(newStream(ch)).Decode(&res)
	if err != nil {
		return nil, err
	}

	if res != nil {
		// tracef("GetPredecessor(Vnode(%q)) => Vnode(%q)", vn.String(), res.Id)
	}
	return t.internalVnode(res), nil
}

func (t *transport) handleGetPredecessor(ch *e3x.Channel) {
	var (
		err   error
		pkt   *lob.Packet
		id    string
		vnode *chord.Vnode
		res   *completeVnode
	)

	defer ch.Close()

	pkt, err = ch.ReadPacket()
	if err != nil {
		// log error
		// tracef("error: %s", err)
		return
	}

	id, _ = pkt.Header().GetString("vn")
	rpc := t.lookupRPC(id)
	if rpc == nil {
		// log
		// tracef("error: %s", "no RPC")
		return
	}

	vnode, err = rpc.GetPredecessor()
	if err != nil {
		// log
		// tracef("error: %s", err)
		return
	}

	res = t.completeVnode(vnode)
	err = json.NewEncoder(newStream(ch)).Encode(&res)
	if err != nil {
		// log
		// tracef("error: %s", err)
		return
	}

	if res != nil {
		// tracef("handle.GetPredecessor(Vnode(%q)) => Vnode(%q)", id, res.Id)
	}
}

// Notify our successor of ourselves
func (t *transport) Notify(target, self *chord.Vnode) ([]*chord.Vnode, error) {
	var (
		addr   *e3x.Addr
		ch     *e3x.Channel
		stream io.ReadWriteCloser
		res    []*completeVnode
		err    error

		req = struct {
			Target string
			Self   *completeVnode
		}{target.String(), t.completeVnode(self)}
	)

	addr = t.lookupAddr(hashname.H(target.Host))
	if addr == nil {
		return nil, e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.notify", true)
	if err != nil {
		return nil, err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	stream = newStream(ch)

	err = json.NewEncoder(stream).Encode(&req)
	if err != nil {
		return nil, err
	}

	err = json.NewDecoder(stream).Decode(&res)
	if err != nil {
		return nil, err
	}

	// tracef("Notify(target:Vnode(%q), self:Vnode(%q)) => []Vnode(%v)", target.String(), self.String(), res)
	return t.internalVnodes(res), nil
}

func (t *transport) handleNotify(ch *e3x.Channel) {
	var (
		err    error
		stream io.ReadWriteCloser
		req    struct {
			Target string
			Self   *completeVnode
		}
		vnodes []*chord.Vnode
		res    []*completeVnode
	)

	defer ch.Close()

	stream = newStream(ch)

	err = json.NewDecoder(stream).Decode(&req)
	if err != nil {
		// log
		// tracef("(Notify) error: %s", err)
		return
	}

	rpc := t.lookupRPC(req.Target)
	if rpc == nil {
		// log
		// tracef("(Notify) error: %s", "no RPC")
		return
	}

	vnodes, err = rpc.Notify(t.internalVnode(req.Self))
	if err != nil {
		// log
		// tracef("(Notify) error: %s", err)
		return
	}

	res = t.completeVnodes(vnodes)

	err = json.NewEncoder(stream).Encode(&res)
	if err != nil {
		// log
		// tracef("(Notify) error: %s", err)
		return
	}

	// tracef("handle.Notify(target:Vnode(%q), self:Vnode(%q)) => []Vnode(%v)", req.Target, req.Self.Id, res)
}

// Find a successor
func (t *transport) FindSuccessors(vn *chord.Vnode, n int, k []byte) ([]*chord.Vnode, error) {
	var (
		addr   *e3x.Addr
		ch     *e3x.Channel
		stream io.ReadWriteCloser
		res    []*completeVnode
		err    error

		req = struct {
			Target string
			N      int
			K      []byte
		}{vn.String(), n, k}
	)

	// tracef("FindSuccessors(target:Vnode(%q))", vn.String())

	addr = t.lookupAddr(hashname.H(vn.Host))
	if addr == nil {
		return nil, e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.successors.find", true)
	if err != nil {
		return nil, err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	stream = newStream(ch)

	err = json.NewEncoder(stream).Encode(&req)
	if err != nil {
		// tracef("(FindSuccessors) error: %s", err)
		return nil, err
	}

	err = json.NewDecoder(stream).Decode(&res)
	if err != nil {
		// tracef("(FindSuccessors) error: %s", err)
		return nil, err
	}

	return t.internalVnodes(res), nil
}

func (t *transport) handleFindSuccessors(ch *e3x.Channel) {
	var (
		err    error
		stream io.ReadWriteCloser
		req    struct {
			Target string
			N      int
			K      []byte
		}
		res []*chord.Vnode
	)

	defer ch.Close()

	stream = newStream(ch)

	err = json.NewDecoder(stream).Decode(&req)
	if err != nil {
		// log
		// tracef("(FindSuccessors) error: %s", err)
		return
	}

	rpc := t.lookupRPC(req.Target)
	if rpc != nil {
		res, err = rpc.FindSuccessors(req.N, req.K)
		if err != nil {
			// log
			// tracef("(FindSuccessors) error: %s", err)
			return
		}
	}

	err = json.NewEncoder(stream).Encode(t.completeVnodes(res))
	if err != nil {
		// log
		// tracef("(FindSuccessors) error: %s", err)
		return
	}
}

// Clears a predecessor if it matches a given vnode. Used to leave.
func (t *transport) ClearPredecessor(target, self *chord.Vnode) error {
	var (
		addr   *e3x.Addr
		ch     *e3x.Channel
		stream io.ReadWriteCloser
		err    error

		req = struct {
			Target string
			Self   *completeVnode
		}{target.String(), t.completeVnode(self)}
	)

	addr = t.lookupAddr(hashname.H(target.Host))
	if addr == nil {
		return e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.predecessor.clear", true)
	if err != nil {
		return err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	stream = newStream(ch)

	err = json.NewEncoder(stream).Encode(&req)
	if err != nil {
		return err
	}

	return nil
}

func (t *transport) handleClearPredecessor(ch *e3x.Channel) {
	var (
		err    error
		stream io.ReadWriteCloser
		req    struct {
			Target string
			Self   *completeVnode
		}
	)

	defer ch.Close()

	stream = newStream(ch)

	err = json.NewDecoder(stream).Decode(&req)
	if err != nil {
		// log
		// tracef("(ClearPredecessor) error: %s", err)
		return
	}

	rpc := t.lookupRPC(req.Target)
	if rpc == nil {
		// log
		// tracef("(ClearPredecessor) error: %s", "no RPC")
		return
	}

	err = rpc.ClearPredecessor(t.internalVnode(req.Self))
	if err != nil {
		// log
		// tracef("(ClearPredecessor) error: %s", err)
		return
	}
}

// Instructs a node to skip a given successor. Used to leave.
func (t *transport) SkipSuccessor(target, self *chord.Vnode) error {
	var (
		addr   *e3x.Addr
		ch     *e3x.Channel
		stream io.ReadWriteCloser
		err    error

		req = struct {
			Target string
			Self   *completeVnode
		}{target.String(), t.completeVnode(self)}
	)

	addr = t.lookupAddr(hashname.H(target.Host))
	if addr == nil {
		return e3x.ErrNoAddress
	}

	ch, err = t.e.Open(addr, "chord.successor.skip", true)
	if err != nil {
		return err
	}

	defer ch.Close()

	// ch.SetReadDeadline(time.Now().Add(30*time.Second))
	// ch.SetWriteDeadline(time.Now().Add(30*time.Second))

	stream = newStream(ch)

	err = json.NewEncoder(stream).Encode(&req)
	if err != nil {
		return err
	}

	return nil
}

func (t *transport) handleSkipSuccessor(ch *e3x.Channel) {
	var (
		err    error
		stream io.ReadWriteCloser
		req    struct {
			Target string
			Self   *completeVnode
		}
	)

	defer ch.Close()

	stream = newStream(ch)

	err = json.NewDecoder(stream).Decode(&req)
	if err != nil {
		// log
		// tracef("(SkipSuccessor) error: %s", err)
		return
	}

	rpc := t.lookupRPC(req.Target)
	if rpc == nil {
		// log
		// tracef("(SkipSuccessor) error: %s", "no RPC")
		return
	}

	err = rpc.SkipSuccessor(t.internalVnode(req.Self))
	if err != nil {
		// log
		// tracef("(SkipSuccessor) error: %s", err)
		return
	}
}

// Register for an RPC callbacks
func (t *transport) Register(vn *chord.Vnode, rpc chord.VnodeRPC) {
	// tracef("Register(Vnode(%q), VnodeRPC(%p))", vn.String(), rpc)
	t.mtx.Lock()
	defer t.mtx.Unlock()

	t.localVnodes[vn.String()] = localRPC{vn, rpc}
}

func (c *completeVnode) String() string {
	return c.Id
}
