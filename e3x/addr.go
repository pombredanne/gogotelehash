package e3x

import (
	"errors"

	"bitbucket.org/simonmenke/go-telehash/e3x/cipherset"
	"bitbucket.org/simonmenke/go-telehash/hashname"
	"bitbucket.org/simonmenke/go-telehash/transports"
)

var ErrNoKeys = errors.New("e3x: no keys")
var ErrNoAddress = errors.New("e3x: no addresses")

type Addr struct {
	hashname hashname.H
	keys     cipherset.Keys
	parts    cipherset.Parts
	addrs    []transports.ResolvedAddr
}

func NewAddr(keys cipherset.Keys, parts cipherset.Parts, addrs []transports.ResolvedAddr) (*Addr, error) {
	var err error

	addr := &Addr{
		keys:  keys,
		parts: parts,
		addrs: addrs,
	}

	if len(addr.keys) == 0 {
		return nil, ErrNoKeys
	}

	if len(addr.addrs) == 0 {
		return nil, ErrNoAddress
	}

	if addr.parts == nil {
		addr.parts = make(cipherset.Parts, len(addr.keys))
	}

	for csid, part := range hashname.PartsFromKeys(addr.keys) {
		addr.parts[csid] = part
	}

	addr.hashname, err = hashname.FromIntermediates(addr.parts)
	if err != nil {
		return nil, err
	}

	return addr, nil
}

func (a *Addr) Hashname() hashname.H {
	return a.hashname
}

func (a *Addr) String() string {
	return string(a.hashname)
}
