package vmcontext

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/filecoin-project/go-state-types/manifest"
	"github.com/filecoin-project/go-state-types/network"
	rt5 "github.com/filecoin-project/specs-actors/v5/actors/runtime"
	"github.com/ipfs/go-cid"
	ipfscbor "github.com/ipfs/go-ipld-cbor"

	actorstypes "github.com/filecoin-project/go-state-types/actors"
	"github.com/filecoin-project/venus/pkg/vm/dispatch"
	"github.com/filecoin-project/venus/pkg/vm/gas"
	"github.com/filecoin-project/venus/pkg/vm/runtime"
	"github.com/filecoin-project/venus/venus-shared/actors"
	"github.com/filecoin-project/venus/venus-shared/actors/aerrors"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin"
	"github.com/filecoin-project/venus/venus-shared/actors/builtin/account"
	init_ "github.com/filecoin-project/venus/venus-shared/actors/builtin/init"
	"github.com/filecoin-project/venus/venus-shared/types"
)

var gasOnActorExec = gas.NewGasCharge("OnActorExec", 0, 0)

// Context for a top-level invocation sequence.
type topLevelContext struct {
	originatorStableAddress address.Address // Stable (public key) address of the top-level message sender.
	originatorCallSeq       uint64          // Call sequence number of the top-level message.
	newActorAddressCount    uint64          // Count of calls To NewActorAddress (mutable).
}

// Context for an individual message invocation, including inter-actor sends.
type invocationContext struct {
	vm                *LegacyVM
	topLevel          *topLevelContext
	originMsg         VmMessage // msg not trasfer from and to address
	msg               VmMessage // The message being processed
	gasTank           *gas.GasTracker
	randSource        HeadChainRandomness
	isCallerValidated bool
	depth             uint64
	allowSideEffects  bool
	stateHandle       internalActorStateHandle
	gasIpld           ipfscbor.IpldStore
}

type internalActorStateHandle interface {
	rt5.StateHandle
}

func newInvocationContext(rt *LegacyVM, gasIpld ipfscbor.IpldStore, topLevel *topLevelContext, msg VmMessage,
	gasTank *gas.GasTracker, randSource HeadChainRandomness, parent *invocationContext,
) invocationContext {
	orginMsg := msg
	ctx := invocationContext{
		vm:                rt,
		topLevel:          topLevel,
		originMsg:         orginMsg,
		gasTank:           gasTank,
		randSource:        randSource,
		isCallerValidated: false,
		depth:             0,
		allowSideEffects:  true,
		stateHandle:       nil,
		gasIpld:           gasIpld,
	}

	if parent != nil {
		// TODO: The version check here should be unnecessary, but we can wait to take it out
		if !parent.allowSideEffects && rt.NetworkVersion() >= network.Version7 {
			runtime.Abortf(exitcode.SysErrForbidden, "internal calls currently disabled")
		}
		// ctx.gasUsed = parent.gasUsed
		// ctx.origin = parent.origin
		// ctx.originNonce = parent.originNonce
		// ctx.numActorsCreated = parent.numActorsCreated
		ctx.depth = parent.depth + 1
	}

	if ctx.depth > MaxCallDepth && rt.NetworkVersion() >= network.Version6 {
		runtime.Abortf(exitcode.SysErrForbidden, "message execution exceeds call depth")
	}

	// Note: the toActor and stateHandle are loaded during the `invoke()`
	resF, ok := rt.normalizeAddress(msg.From)
	if !ok {
		runtime.Abortf(exitcode.SysErrInvalidReceiver, "resolve msg.From [%s] address failed", msg.From)
	}
	msg.From = resF

	if rt.NetworkVersion() > network.Version3 {
		resT, _ := rt.normalizeAddress(msg.To)
		// may be set to undef if recipient doesn't exist yet
		msg.To = resT
	}
	ctx.msg = msg

	return ctx
}

type stateHandleContext invocationContext

func (shc *stateHandleContext) AllowSideEffects(allow bool) {
	shc.allowSideEffects = allow
}

func (shc *stateHandleContext) Create(obj cbor.Marshaler) cid.Cid {
	actr := shc.loadActor()
	if actr.Head.Defined() {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "failed To construct actor stateView: already initialized")
	}
	c := shc.store().StorePut(obj)
	actr.Head = c
	shc.storeActor(actr)
	return c
}

func (shc *stateHandleContext) Load(obj cbor.Unmarshaler) cid.Cid {
	// The actor must be loaded From store every time since the stateView may have changed via a different stateView handle
	// (e.g. in a recursive call).
	actr := shc.loadActor()
	c := actr.Head
	if !c.Defined() {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "failed To load undefined stateView, must construct first")
	}
	found := shc.store().StoreGet(c, obj)
	if !found {
		panic(fmt.Errorf("failed To load stateView for actor %s, CID %s", shc.msg.To, c))
	}
	return c
}

func (shc *stateHandleContext) Replace(expected cid.Cid, obj cbor.Marshaler) cid.Cid {
	actr := shc.loadActor()
	if !actr.Head.Equals(expected) {
		panic(fmt.Errorf("unexpected prior stateView %s for actor %s, expected %s", actr.Head, shc.msg.To, expected))
	}
	c := shc.store().StorePut(obj)
	actr.Head = c
	shc.storeActor(actr)
	return c
}

func (shc *stateHandleContext) store() rt5.Store {
	return ((*invocationContext)(shc)).Store()
}

func (shc *stateHandleContext) loadActor() *types.Actor {
	entry, found, err := shc.vm.State.GetActor(shc.vm.context, shc.originMsg.To)
	if err != nil {
		panic(err)
	}
	if !found {
		panic(fmt.Errorf("failed To find actor %s for stateView", shc.originMsg.To))
	}
	return entry
}

func (shc *stateHandleContext) storeActor(actr *types.Actor) {
	err := shc.vm.State.SetActor(shc.vm.context, shc.originMsg.To, actr)
	if err != nil {
		panic(err)
	}
}

// runtime aborts are trapped by invoke, it will always return an exit code.
func (ctx *invocationContext) invoke() (ret []byte, errcode exitcode.ExitCode) {
	// Checkpoint stateView, for restoration on revert
	// Note that changes prior To invocation (sequence number bump and gas prepayment) persist even if invocation fails.
	err := ctx.vm.snapshot()
	if err != nil {
		panic(err)
	}
	defer ctx.vm.clearSnapshot()

	// Install handler for abort, which rolls back all stateView changes From this and any nested invocations.
	// This is the only path by which a non-OK exit code may be returned.
	defer func() {
		if r := recover(); r != nil {

			if err := ctx.vm.revert(); err != nil {
				panic(err)
			}
			switch e := r.(type) {
			case runtime.ExecutionPanic:
				p := e

				vmlog.Warnw("Abort during actor execution.",
					"errorMessage", p,
					"exitCode", p.Code(),
					"sender", ctx.originMsg.From,
					"receiver", ctx.originMsg.To,
					"methodNum", ctx.originMsg.Method,
					"Value", ctx.originMsg.Value,
					"gasLimit", ctx.gasTank.GasAvailable)
				ret = []byte{} // The Empty here should never be used, but slightly safer than zero Value.
				errcode = p.Code()
			default:
				errcode = 1
				ret = []byte{}
				// do not trap unknown panics
				vmlog.Errorf("spec actors failure: %s", r)
				// debug.PrintStack()
			}
		}
	}()

	// pre-dispatch
	// 1. charge gas for message invocation
	// 2. load target actor
	// 3. transfer optional funds
	// 4. short-circuit _Send_ Method
	// 5. create target stateView handle
	// assert From address is an ID address.
	if ctx.msg.From.Protocol() != address.ID {
		panic("bad code: sender address MUST be an ID address at invocation time")
	}

	// 1. load target actor
	// Note: we replace the "To" address with the normalized version
	toActor, toIDAddr := ctx.resolveTarget(ctx.originMsg.To)
	if ctx.vm.NetworkVersion() > network.Version3 {
		ctx.msg.To = toIDAddr
	}

	// 2. charge gas for msg
	ctx.gasTank.Charge(ctx.vm.pricelist.OnMethodInvocation(ctx.originMsg.Value, ctx.originMsg.Method), "Method invocation")

	// 3. transfer funds carried by the msg
	if !ctx.originMsg.Value.Nil() && !ctx.originMsg.Value.IsZero() {
		ctx.vm.transfer(ctx.msg.From, toIDAddr, ctx.originMsg.Value, ctx.vm.NetworkVersion())
	}

	// 4. if we are just sending funds, there is nothing else To do.
	if ctx.originMsg.Method == builtin.MethodSend {
		return nil, exitcode.Ok
	}

	actorImpl := ctx.vm.getActorImpl(toActor.Code, ctx.Runtime())

	// 5. create target stateView handle
	stateHandle := newActorStateHandle((*stateHandleContext)(ctx))
	ctx.stateHandle = &stateHandle

	// dispatch
	adapter := newRuntimeAdapter(ctx) // runtimeAdapter{ctx: ctx}
	var extErr *dispatch.ExcuteError
	ret, extErr = actorImpl.Dispatch(ctx.originMsg.Method, ctx.vm.NetworkVersion(), adapter, ctx.originMsg.Params)
	if extErr != nil {
		runtime.Abortf(extErr.ExitCode(), extErr.Error())
	}

	// post-dispatch
	// 1. check caller was validated
	// 2. check stateView manipulation was valid
	// 4. success!

	// 1. check caller was validated
	if !ctx.isCallerValidated {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "Caller MUST be validated during Method execution")
	}

	// Reset To pre-invocation stateView
	ctx.stateHandle = nil

	// 3. success!
	return ret, exitcode.Ok
}

// resolveTarget loads and actor and returns its ActorID address.
//
// If the target actor does not exist, and the target address is a pub-key address,
// a new account actor will be created.
// Otherwise, this Method will abort execution.
func (ctx *invocationContext) resolveTarget(target address.Address) (*types.Actor, address.Address) {
	// resolve the target address via the InitActor, and attempt To load stateView.
	initActorEntry, found, err := ctx.vm.State.GetActor(ctx.vm.context, init_.Address)
	if err != nil {
		panic(err)
	}
	if !found {
		runtime.Abort(exitcode.SysErrSenderInvalid)
	}

	if target == init_.Address {
		return initActorEntry, target
	}

	// get init State
	state, err := init_.Load(ctx.vm.ContextStore(), initActorEntry)
	if err != nil {
		panic(err)
	}

	// lookup the ActorID based on the address

	_, found, err = ctx.vm.State.GetActor(ctx.vm.context, target)
	if err != nil {
		panic(err)
	}
	//nolint
	if !found {
		// Charge gas now that easy checks are done

		ctx.gasTank.Charge(ctx.vm.pricelist.OnCreateActor(), "CreateActor  address %s", target)
		// actor does not exist, create an account actor
		// - precond: address must be a pub-key
		// - sent init actor a msg To create the new account
		targetIDAddr, err := ctx.vm.State.RegisterNewAddress(target)
		if err != nil {
			panic(err)
		}

		if target.Protocol() != address.SECP256K1 && target.Protocol() != address.BLS {
			// Don't implicitly create an account actor for an address without an associated key.
			runtime.Abort(exitcode.SysErrInvalidReceiver)
		}
		ver, err := actorstypes.VersionForNetwork(ctx.vm.NetworkVersion())
		if err != nil {
			panic(err)
		}
		actorCode, found := actors.GetActorCodeID(ver, manifest.AccountKey)
		if !found {
			panic(fmt.Errorf("failed to get account actor code ID for actors version %d", ver))
		}
		ctx.CreateActor(actorCode, targetIDAddr)

		// call constructor on account
		newMsg := VmMessage{
			From:   builtin.SystemActorAddr,
			To:     targetIDAddr,
			Value:  big.Zero(),
			Method: account.Methods.Constructor,
			// use original address as constructor Params
			// Note: constructor takes a pointer
			Params: &target,
		}

		newCtx := newInvocationContext(ctx.vm, ctx.gasIpld, ctx.topLevel, newMsg, ctx.gasTank, ctx.randSource, ctx)
		_, code := newCtx.invoke()
		if code.IsError() {
			// we failed To construct an account actor..
			runtime.Abort(code)
		}

		// load actor
		targetActor, _, err := ctx.vm.State.GetActor(ctx.vm.context, target)
		if err != nil {
			panic(err)
		}
		return targetActor, targetIDAddr
	} else {
		// load id address
		targetIDAddr, found, err := state.ResolveAddress(target)
		if err != nil {
			panic(err)
		}

		if !found {
			panic(fmt.Errorf("unreachable: actor is supposed To exist but it does not. addr: %s, idAddr: %s", target, targetIDAddr))
		}

		// load actor
		targetActor, found, err := ctx.vm.State.GetActor(ctx.vm.context, targetIDAddr)
		if err != nil {
			panic(err)
		}

		if !found {
			runtime.Abort(exitcode.SysErrInvalidReceiver)
		}

		return targetActor, targetIDAddr
	}
}

func (ctx *invocationContext) resolveToDeterministicAddress(addr address.Address) (address.Address, error) {
	return ResolveToDeterministicAddress(ctx.vm.context, ctx.vm.State, addr, ctx.vm.store)
}

// implement runtime.InvocationContext for invocationContext
var _ runtime.InvocationContext = (*invocationContext)(nil)

// Runtime implements runtime.InvocationContext.
func (ctx *invocationContext) Runtime() runtime.Runtime {
	return ctx.vm
}

// Store implements runtime.Runtime.
func (ctx *invocationContext) Store() rt5.Store {
	return NewActorStorage(ctx.vm.context, ctx.gasIpld, ctx.gasTank, ctx.vm.pricelist)
}

// Message implements runtime.InvocationContext.
func (ctx *invocationContext) Message() rt5.Message {
	return ctx.msg
}

// ValidateCaller implements runtime.InvocationContext.
func (ctx *invocationContext) ValidateCaller(pattern runtime.CallerPattern) {
	if ctx.isCallerValidated {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "Method must validate caller identity exactly once")
	}
	if !pattern.IsMatch((*patternContext2)(ctx)) {
		runtime.Abortf(exitcode.SysErrForbidden, "Method invoked by incorrect caller")
	}
	ctx.isCallerValidated = true
}

// State implements runtime.InvocationContext.
func (ctx *invocationContext) State() rt5.StateHandle {
	return ctx.stateHandle
}

// Send implements runtime.InvocationContext.
func (ctx *invocationContext) Send(toAddr address.Address, methodNum abi.MethodNum, params cbor.Marshaler, value abi.TokenAmount, out cbor.Er) exitcode.ExitCode {
	// check if side-effects are allowed
	if !ctx.allowSideEffects {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "Calling Send() is not allowed during side-effect lock")
	}
	// prepare
	// 1. alias fromActor
	from := ctx.msg.To

	// 2. build internal message
	newMsg := VmMessage{
		From:   from,
		To:     toAddr,
		Value:  value,
		Method: methodNum,
		Params: params,
	}

	// 3. build new context
	newCtx := newInvocationContext(ctx.vm, ctx.gasIpld, ctx.topLevel, newMsg, ctx.gasTank, ctx.randSource, ctx)
	// 4. invoke
	ret, code := newCtx.invoke()
	if code == 0 {
		_ = ctx.gasTank.TryCharge(gasOnActorExec)
		if err := out.UnmarshalCBOR(bytes.NewReader(ret)); err != nil {
			runtime.Abortf(exitcode.ErrSerialization, "failed To unmarshal return Value: %s", err)
		}
	}
	return code
}

// Balance implements runtime.InvocationContext.
func (ctx *invocationContext) Balance() abi.TokenAmount {
	toActor, found, err := ctx.vm.State.GetActor(ctx.vm.context, ctx.originMsg.To)
	if err != nil {
		panic(fmt.Errorf("cannot find to actor %v", err))
	}
	if !found {
		return abi.NewTokenAmount(0)
	}
	return toActor.Balance
}

// implement runtime.InvocationContext for invocationContext
var _ runtime.ExtendedInvocationContext = (*invocationContext)(nil)

// NewActorAddress predicts the address of the next actor created by this address.
//
// Code is adapted from vm.Runtime#NewActorAddress()
func (ctx *invocationContext) NewActorAddress() address.Address {
	buf := new(bytes.Buffer)
	origin, err := ctx.resolveToDeterministicAddress(ctx.topLevel.originatorStableAddress)
	if err != nil {
		panic(err)
	}

	err = origin.MarshalCBOR(buf)
	if err != nil {
		panic(err)
	}

	err = binary.Write(buf, binary.BigEndian, ctx.topLevel.originatorCallSeq)
	if err != nil {
		panic(err)
	}

	err = binary.Write(buf, binary.BigEndian, ctx.topLevel.newActorAddressCount)
	if err != nil {
		panic(err)
	}

	actorAddress, err := address.NewActorAddress(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return actorAddress
}

// CreateActor implements runtime.ExtendedInvocationContext.
func (ctx *invocationContext) CreateActor(codeID cid.Cid, addr address.Address) {
	if addr == address.Undef && ctx.vm.NetworkVersion() >= network.Version7 {
		runtime.Abortf(exitcode.SysErrorIllegalArgument, "CreateActor with Undef address")
	}

	vmlog.Debugf("creating actor, friendly-name: %s, code: %s, addr: %s\n", builtin.ActorNameByCode(codeID), codeID, addr)

	// Check existing address. If nothing there, create empty actor.
	// Note: we are storing the actors by ActorID *address*
	_, found, err := ctx.vm.State.GetActor(ctx.vm.context, addr)
	if err != nil {
		panic(err)
	}
	if found {
		runtime.Abortf(exitcode.SysErrorIllegalArgument, "Actor address already exists")
	}

	newActor := &types.Actor{
		// make this the right 'type' of actor
		Code:             codeID,
		Balance:          abi.NewTokenAmount(0),
		Head:             EmptyObjectCid,
		Nonce:            0,
		DelegatedAddress: &addr,
	}
	if err := ctx.vm.State.SetActor(ctx.vm.context, addr, newActor); err != nil {
		panic(err)
	}

	_ = ctx.gasTank.TryCharge(gasOnActorExec)
}

// DeleteActor implements runtime.ExtendedInvocationContext.
func (ctx *invocationContext) DeleteActor(beneficiary address.Address) {
	receiver := ctx.originMsg.To
	ctx.gasTank.Charge(ctx.vm.pricelist.OnDeleteActor(), "DeleteActor %s", receiver)
	receiverActor, found, err := ctx.vm.State.GetActor(ctx.vm.context, receiver)
	if err != nil {
		if errors.Is(err, types.ErrActorNotFound) {
			runtime.Abortf(exitcode.SysErrorIllegalActor, "failed to load actor in delete actor: %s", err)
		}
		panic(aerrors.Fatalf("failed to get actor: %s", err))
	}

	if !found {
		runtime.Abortf(exitcode.SysErrorIllegalActor, "delete non-existent actor %v", receiverActor)
	}

	if !receiverActor.Balance.IsZero() {
		// TODO: Should be safe to drop the version-check,
		//  since only the paych actor called this pre-version 7, but let's leave it for now
		if ctx.vm.NetworkVersion() >= network.Version7 {
			beneficiaryID, found := ctx.vm.normalizeAddress(beneficiary)
			if !found {
				runtime.Abortf(exitcode.SysErrorIllegalArgument, "beneficiary doesn't exist")
			}

			if beneficiaryID == receiver {
				runtime.Abortf(exitcode.SysErrorIllegalArgument, "benefactor cannot be beneficiary")
			}
		}

		// Transfer the executing actor's balance to the beneficiary
		ctx.vm.transfer(receiver, beneficiary, receiverActor.Balance, ctx.vm.NetworkVersion())
	}

	if err := ctx.vm.State.DeleteActor(ctx.vm.context, receiver); err != nil {
		panic(aerrors.Fatalf("failed to delete actor: %s", err))
	}

	_ = ctx.gasTank.TryCharge(gasOnActorExec)
}

func (ctx *invocationContext) stateView() SyscallsStateView {
	// The stateView tree's root is not committed until the end of a tipset, so we can't use the external stateView view
	// type for this implementation.
	// Maybe we could re-work it To use a root HAMT node rather than root CID.
	return newSyscallsStateView(ctx, ctx.vm)
}

// patternContext2 implements the PatternContext
type patternContext2 invocationContext

var _ runtime.PatternContext = (*patternContext2)(nil)

func (ctx *patternContext2) CallerCode() cid.Cid {
	toActor, found, err := ctx.vm.State.GetActor(ctx.vm.context, ctx.originMsg.From)
	if err != nil || !found {
		panic(fmt.Errorf("cannt find to actor %v", err))
	}
	return toActor.Code
}

func (ctx *patternContext2) CallerAddr() address.Address {
	return ctx.msg.From
}
