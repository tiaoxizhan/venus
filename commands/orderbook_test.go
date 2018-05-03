package commands

import (
	"fmt"
	"testing"

	"github.com/filecoin-project/go-filecoin/address"
	"github.com/stretchr/testify/assert"
)

func TestBidList(t *testing.T) {
	assert := assert.New(t)

	d := NewDaemon(t).Start()
	defer d.ShutdownSuccess()

	d.CreateWalletAddr()

	for i := 0; i < 10; i++ {
		d.RunSuccess("client", "add-bid", "1", fmt.Sprintf("%d", i),
			"--from", address.TestAddress.String(),
		)
	}

	for i := 0; i < 10; i++ {
		d.RunSuccess("mining", "once")
	}

	list := d.RunSuccess("orderbook", "bids").ReadStdout()
	for i := 0; i < 10; i++ {
		assert.Contains(list, fmt.Sprintf("\"price\":\"%d\"", i))
	}

}

func TestAskList(t *testing.T) {
	assert := assert.New(t)

	d := NewDaemon(t).Start()
	defer d.ShutdownSuccess()

	minerAddr := d.CreateMinerAddr()

	for i := 0; i < 10; i++ {
		d.RunSuccess(
			"miner", "add-ask",
			"--from", d.Config().Mining.RewardAddress.String(),
			minerAddr.String(), "1", fmt.Sprintf("%d", i),
		)
	}

	d.RunSuccess("mining", "once")

	list := d.RunSuccess("orderbook", "asks").ReadStdout()
	for i := 0; i < 10; i++ {
		assert.Contains(list, fmt.Sprintf("\"price\":\"%d\"", i))
	}

}

// TODO when PR 190 merges
func TestDealList(t *testing.T) {
}
