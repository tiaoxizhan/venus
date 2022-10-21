// FETCHED FROM LOTUS: builtin/init/actor.go.template

package init

import (
	"fmt"

	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/venus/venus-shared/actors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/venus/venus-shared/actors/adt"
	types "github.com/filecoin-project/venus/venus-shared/internal"

	builtin0 "github.com/filecoin-project/specs-actors/actors/builtin"

	builtin2 "github.com/filecoin-project/specs-actors/v2/actors/builtin"

	builtin3 "github.com/filecoin-project/specs-actors/v3/actors/builtin"

	builtin4 "github.com/filecoin-project/specs-actors/v4/actors/builtin"

	builtin5 "github.com/filecoin-project/specs-actors/v5/actors/builtin"

	builtin6 "github.com/filecoin-project/specs-actors/v6/actors/builtin"

	builtin7 "github.com/filecoin-project/specs-actors/v7/actors/builtin"

	builtin9 "github.com/filecoin-project/go-state-types/builtin"
)

var (
	Address = builtin9.InitActorAddr
	Methods = builtin9.MethodsInit
)

func Load(store adt.Store, act *types.Actor) (State, error) {
	if name, av, ok := actors.GetActorMetaByCode(act.Code); ok {
		if name != actors.InitKey {
			return nil, fmt.Errorf("actor code is not init: %s", name)
		}

		switch av {

		case actorstypes.Version8:
			return load8(store, act.Head)

		case actorstypes.Version9:
			return load9(store, act.Head)

		}
	}

	switch act.Code {

	case builtin0.InitActorCodeID:
		return load0(store, act.Head)

	case builtin2.InitActorCodeID:
		return load2(store, act.Head)

	case builtin3.InitActorCodeID:
		return load3(store, act.Head)

	case builtin4.InitActorCodeID:
		return load4(store, act.Head)

	case builtin5.InitActorCodeID:
		return load5(store, act.Head)

	case builtin6.InitActorCodeID:
		return load6(store, act.Head)

	case builtin7.InitActorCodeID:
		return load7(store, act.Head)

	}

	return nil, fmt.Errorf("unknown actor code %s", act.Code)
}

func MakeState(store adt.Store, av actorstypes.Version, networkName string) (State, error) {
	switch av {

	case actorstypes.Version0:
		return make0(store, networkName)

	case actorstypes.Version2:
		return make2(store, networkName)

	case actorstypes.Version3:
		return make3(store, networkName)

	case actorstypes.Version4:
		return make4(store, networkName)

	case actorstypes.Version5:
		return make5(store, networkName)

	case actorstypes.Version6:
		return make6(store, networkName)

	case actorstypes.Version7:
		return make7(store, networkName)

	case actorstypes.Version8:
		return make8(store, networkName)

	case actorstypes.Version9:
		return make9(store, networkName)

	}
	return nil, fmt.Errorf("unknown actor version %d", av)
}

type State interface {
	cbor.Marshaler

	ResolveAddress(address address.Address) (address.Address, bool, error)
	MapAddressToNewID(address address.Address) (address.Address, error)
	NetworkName() (string, error)

	ForEachActor(func(id abi.ActorID, address address.Address) error) error

	// Remove exists to support tooling that manipulates state for testing.
	// It should not be used in production code, as init actor entries are
	// immutable.
	Remove(addrs ...address.Address) error

	// Sets the network's name. This should only be used on upgrade/fork.
	SetNetworkName(name string) error

	// Sets the next ID for the init actor. This should only be used for testing.
	SetNextID(id abi.ActorID) error

	// Sets the address map for the init actor. This should only be used for testing.
	SetAddressMap(mcid cid.Cid) error

	AddressMap() (adt.Map, error)
	GetState() interface{}
}
