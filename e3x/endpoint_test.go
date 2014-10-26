package e3x

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"bitbucket.org/simonmenke/go-telehash/e3x/cipherset"
	_ "bitbucket.org/simonmenke/go-telehash/e3x/cipherset/cs3a"
	"bitbucket.org/simonmenke/go-telehash/transports/mux"
	"bitbucket.org/simonmenke/go-telehash/transports/udp"
	"bitbucket.org/simonmenke/go-telehash/util/logs"
)

func TestSimpleEndpoint(t *testing.T) {
	logs.ResetLogger()

	if testing.Short() {
		t.Skip("this is a long running test.")
	}

	assert := assert.New(t)

	ka, err := cipherset.GenerateKey(0x3a)
	assert.NoError(err)

	kb, err := cipherset.GenerateKey(0x3a)
	assert.NoError(err)

	ea := New(cipherset.Keys{0x3a: ka}, mux.Config{udp.Config{}})
	eb := New(cipherset.Keys{0x3a: kb}, mux.Config{udp.Config{}})

	registerEventLoggers(ea, t)
	registerEventLoggers(eb, t)

	err = ea.Start()
	assert.NoError(err)

	err = eb.Start()
	assert.NoError(err)

	time.Sleep(1 * time.Second)

	identA, err := ea.LocalIdent()
	assert.NoError(err)

	identB, err := eb.LocalIdent()
	assert.NoError(err)

	tracef("HELLO")
	_, err = ea.Dial(identB)
	assert.NoError(err)

	_, err = ea.Dial(identB)
	assert.NoError(err)

	_, err = eb.Dial(identA)
	assert.NoError(err)

	time.Sleep(2*time.Minute + 10*time.Second)
	tracef("BYE")

	err = ea.Stop()
	assert.NoError(err)

	err = eb.Stop()
	assert.NoError(err)
}
