package dispatch

import (
	"bytes"
	"fmt"
	"reflect"

	builtin2 "github.com/filecoin-project/specs-actors/v2/actors/builtin"
	builtin3 "github.com/filecoin-project/specs-actors/v3/actors/builtin"

	"github.com/filecoin-project/go-state-types/exitcode"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/cbor"
	"github.com/filecoin-project/specs-actors/actors/runtime"
	"github.com/ipfs/go-cid"
)

type SimpleParams struct {
	Name string
}

// Actor is the interface all actors have to implement.
type Actor interface {
	// Exports has a list of method available on the actor.
	Exports() []interface{}
	// Code returns the code ID for this actor.
	Code() cid.Cid

	// State returns a new State object for this actor. This can be used to
	// decode the actor's state.
	State() cbor.Er
}

// Dispatcher allows for dynamic method dispatching on an actor.
type Dispatcher interface {
	// Dispatch will call the given method on the actor and pass the arguments.
	//
	// - The `ctx` argument will be coerced to the type the method expects in its first argument.
	// - If arg1 is `[]byte`, it will attempt to decode the value based on second argument in the target method.
	Dispatch(method abi.MethodNum, nvk network.Version, ctx interface{}, arg1 interface{}) ([]byte, *ExcuteError)
	// Signature is a helper function that returns the signature for a given method.
	//
	// Note: This is intended to be used by tests and tools.
	Signature(method abi.MethodNum) (MethodSignature, *ExcuteError)
}

type actorDispatcher struct {
	code  cid.Cid
	actor Actor
}

type method interface {
	Call(in []reflect.Value) []reflect.Value
	Type() reflect.Type
}

var _ Dispatcher = (*actorDispatcher)(nil)

// Dispatch implements `Dispatcher`.
func (d *actorDispatcher) Dispatch(methodNum abi.MethodNum, nvk network.Version, ctx interface{}, arg1 interface{}) ([]byte, *ExcuteError) {
	// get method signature
	m, err := d.signature(methodNum)
	if err != nil {
		return []byte{}, err
	}

	// build args to pass to the method
	args := []reflect.Value{
		// the ctx will be automatically coerced
		reflect.ValueOf(ctx),
	}

	// Dragons: simplify this to arginterface
	parserByte := func(raw []byte) *ExcuteError {
		obj, err := m.ArgInterface(raw)
		if err != nil {
			ec := exitcode.ErrSerialization
			if nvk < network.Version7 {
				ec = 1
			}
			return NewExcuteError(ec, "fail to decode params")
		}
		args = append(args, reflect.ValueOf(obj))
		return nil
	}

	switch t := arg1.(type) {
	case nil:
		args = append(args, m.ArgNil())
	case []byte:
		err := parserByte(t)
		if err != nil {
			return []byte{}, err
		}
	case builtin2.CBORBytes:
		err := parserByte(t)
		if err != nil {
			return []byte{}, err
		}
	case builtin3.CBORBytes:
		err := parserByte(t)
		if err != nil {
			return []byte{}, err
		}
	case runtime.CBORBytes:
		err := parserByte(t)
		if err != nil {
			return []byte{}, err
		}
	default:
		args = append(args, reflect.ValueOf(arg1))
	}

	// invoke the method
	out := m.method.Call(args)

	// Note: we only support single objects being returned
	/*if len(out) > 1 {  todo nocheck and lotus nocheck too
		return nil, fmt.Errorf("actor method returned more than one object. method: %d, code: %s", methodNum, d.code)
	}*/

	// method returns unit
	// Note: we need to check for `IsNill()` here because Go doesnt work if you do `== nil` on the interface
	if len(out) == 0 || (out[0].Kind() != reflect.Struct && out[0].IsNil()) {
		return nil, nil
	}

	switch ret := out[0].Interface().(type) {
	case []byte:
		return ret, nil
	case *abi.EmptyValue:
		return []byte{}, nil
	case cbor.Marshaler:
		buf := new(bytes.Buffer)
		if err := ret.MarshalCBOR(buf); err != nil {
			return []byte{}, NewExcuteError(exitcode.SysErrSenderStateInvalid, "failed to marshal response to cbor err:%v", err)
		}
		return buf.Bytes(), nil
	case nil:
		return []byte{}, nil
	default:
		return []byte{}, NewExcuteError(exitcode.SysErrInvalidMethod, "could not determine type for response from call")
	}
}

func (d *actorDispatcher) signature(methodID abi.MethodNum) (*methodSignature, *ExcuteError) {
	exports := d.actor.Exports()

	// get method entry
	methodIdx := (uint64)(methodID)
	if len(exports) <= (int)(methodIdx) {
		return nil, NewExcuteError(exitcode.SysErrInvalidMethod, "Method undefined. method: %d, code: %s", methodID, d.code)
	}
	entry := exports[methodIdx]
	if entry == nil {
		return nil, NewExcuteError(exitcode.SysErrInvalidMethod, "Method undefined. method: %d, code: %s", methodID, d.code)
	}

	ventry := reflect.ValueOf(entry)
	return &methodSignature{method: ventry}, nil
}

// Signature implements `Dispatcher`.
func (d *actorDispatcher) Signature(methodNum abi.MethodNum) (MethodSignature, *ExcuteError) {
	return d.signature(methodNum)
}

type ExcuteError struct {
	code exitcode.ExitCode
	msg  string
}

func NewExcuteError(code exitcode.ExitCode, msg string, args ...interface{}) *ExcuteError {
	return &ExcuteError{code: code, msg: fmt.Sprint(msg, args)}
}

func (err *ExcuteError) ExitCode() exitcode.ExitCode {
	return err.code
}

func (err *ExcuteError) Error() string {
	return err.msg

}
