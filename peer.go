package telehash

import (
	"github.com/fd/go-util/log"
	"sync"
	"time"
)

type peer_i interface {
	snd_pkt(*pkt_t) error          // sends a packet over a line if there is one.
	snd_pkt_blocking(*pkt_t) error // like snd_pkt() but wait for a line
}

type peer_t struct {
	addr addr_t

	sw              *Switch
	line            *line_t
	peer_cmd_snd_at time.Time
	open_cmd_snd_at time.Time
	channels        map[string]*channel_t

	mtx sync.RWMutex
	cnd sync.Cond
	log log.Logger
}

func make_peer(sw *Switch, hashname Hashname) *peer_t {
	peer := &peer_t{
		addr:     addr_t{hashname: hashname},
		sw:       sw,
		channels: make(map[string]*channel_t, 100),
		log:      sw.peers.log.Sub(log.INFO, hashname.Short()),
	}

	peer.cnd.L = peer.mtx.RLocker()

	return peer
}

func (p *peer_t) String() string {
	return p.addr.String()
}

func (p *peer_t) open_channel(pkt *pkt_t) (*channel_t, error) {
	channel, err := p.make_channel("", pkt.hdr.Type, true)
	if err != nil {
		return nil, err
	}

	channel.log.Infof("channel[%s:%s](%s -> %s): opened",
		short_hash(channel.channel_id),
		pkt.hdr.Type,
		p.sw.peers.get_local_hashname().Short(),
		p.addr.hashname.Short())

	err = channel.snd_pkt(pkt)
	if err != nil {
		return nil, err
	}

	return channel, nil
}

func (p *peer_t) push_rcv_pkt(pkt *pkt_t) error {
	pkt.addr = p.addr

	if pkt.hdr.C == "" {
		return errInvalidPkt
	}

	// send pkt to existing channel
	if channel := p.channels[pkt.hdr.C]; channel != nil {
		p.log.Debugf("rcv pkt: addr=%s hdr=%+v", p, pkt.hdr)
		return channel.push_rcv_pkt(pkt)
	}

	// open new channel
	if pkt.hdr.Seq.IsSet() && pkt.hdr.Seq.Get() == 0 {
		// first packet in sequence

		if pkt.hdr.Type == "" {
			return errInvalidPkt
		}

		channel, err := p.make_channel(pkt.hdr.C, pkt.hdr.Type, false)
		if err != nil {
			return err
		}

		p.log.Debugf("rcv pkt: addr=%s hdr=%+v", p, pkt.hdr)

		channel.log.Infof("channel[%s:%s](%s -> %s): opened",
			short_hash(channel.channel_id),
			pkt.hdr.Type,
			p.sw.peers.get_local_hashname().Short(),
			p.addr.hashname.Short())

		err = channel.push_rcv_pkt(pkt)
		if err != nil {
			return err
		}

		go channel.run_user_handler()

		return nil
	}

	// else:
	return errInvalidPkt
}

func (p *peer_t) make_channel(id, typ string, initiator bool) (*channel_t, error) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	channel, err := make_channel(p.sw, p, id, typ, initiator)
	if err != nil {
		return nil, err
	}

	p.channels[channel.channel_id] = channel

	return channel, nil
}

func (p *peer_t) snd_pkt(pkt *pkt_t) error {
	p.log.Debugf("snd pkt: addr=%s hdr=%+v", p, pkt.hdr)

	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.line == nil {
		// drop
		return errNoOpenLine
	}

	pkt.addr = p.addr

	return p.line.snd_pkt(pkt)
}

func (p *peer_t) snd_pkt_blocking(pkt *pkt_t) error {
	p.log.Debugf("snd pkt: addr=%s hdr=%+v", p, pkt.hdr)

	p.mtx.RLock()
	defer p.mtx.RUnlock()

	if p.line == nil {
		go p.open_line()
	}

	for p.line == nil {
		p.cnd.Wait()
	}

	pkt.addr = p.addr

	return p.line.snd_pkt(pkt)
}

func (p *peer_t) has_open_line() bool {
	p.mtx.RLock()
	defer p.mtx.RUnlock()

	return p.line != nil
}

func (p *peer_t) activate_line(line *line_t) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.line = line
	p.cnd.Broadcast()
}

func (p *peer_t) deactivate_line(line *line_t) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.line == line {
		p.line = nil
		p.cnd.Broadcast()
	}
}

func (p *peer_t) tick(now time.Time) {
	p.mtx.Lock()
	var (
		channels = make([]*channel_t, 0, len(p.channels))
		closed   []string
	)
	for _, c := range p.channels {
		channels = append(channels, c)
	}
	p.mtx.Unlock()

	for _, c := range channels {
		err := c.snd_ack()
		if err != nil {
			p.log.Debugf("auto-ack: error=%s", err)
		}

		c.detect_rcv_deadline(now)
		c.detect_broken(now)

		if c.is_closed() {
			closed = append(closed, c.channel_id)

			if c.broken {
				c.log.Infof("channel[%s:%s](%s -> %s): broken",
					short_hash(c.channel_id),
					c.channel_type,
					p.sw.peers.get_local_hashname().Short(),
					p.addr.hashname.Short())
			} else {
				c.log.Infof("channel[%s:%s](%s -> %s): closed",
					short_hash(c.channel_id),
					c.channel_type,
					p.sw.peers.get_local_hashname().Short(),
					p.addr.hashname.Short())
			}
		}
	}

	if len(closed) > 0 {
		p.mtx.Lock()
		for _, id := range closed {
			delete(p.channels, id)
		}
		p.mtx.Unlock()
	}
}

func (p *peer_t) open_line() {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.addr.pubkey == nil && !p.addr.via.IsZero() {
		p.peer_open_line()
	} else {
		p.self_open_line()
	}
}

func (p *peer_t) self_open_line() {
	p.open_cmd_snd_at = time.Now()

	err := p.sw.lines._snd_open_pkt(p)
	if err != nil {
		p.log.Debugf("open-line: error=%s", err)
	}
}

func (p *peer_t) peer_open_line() {
	p.peer_cmd_snd_at = time.Now()

	p.send_peer_cmd()
}
