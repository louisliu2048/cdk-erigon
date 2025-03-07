// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package native

import (
	"encoding/json"
	"errors"
	"math/big"
	"sync/atomic"

	"github.com/holiman/uint256"

	libcommon "github.com/gateway-fm/cdk-erigon-lib/common"
	"github.com/gateway-fm/cdk-erigon-lib/common/hexutility"

	"github.com/ledgerwatch/erigon/accounts/abi"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/hexutil"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/tracers"
)

//go:generate go run github.com/fjl/gencodec -type callFrame -field-override callFrameMarshaling -out gen_callframe_json.go

func init() {
	register("callTracer", newCallTracer)
}

type callLog struct {
	Index   uint64            `json:"index"`
	Address libcommon.Address `json:"address"`
	Topics  []libcommon.Hash  `json:"topics"`
	Data    hexutility.Bytes  `json:"data"`
}

type callFrame struct {
	Type     vm.OpCode         `json:"-"`
	From     libcommon.Address `json:"from"`
	Gas      uint64            `json:"gas"`
	GasUsed  uint64            `json:"gasUsed"`
	To       libcommon.Address `json:"to,omitempty" rlp:"optional"`
	Input    []byte            `json:"input" rlp:"optional"`
	Output   []byte            `json:"output,omitempty" rlp:"optional"`
	Error    string            `json:"error,omitempty" rlp:"optional"`
	Revertal string            `json:"revertReason,omitempty"`
	Calls    []callFrame       `json:"calls,omitempty" rlp:"optional"`
	Logs     []callLog         `json:"logs,omitempty" rlp:"optional"`
	// Placed at end on purpose. The RLP will be decoded to 0 instead of
	// nil if there are non-empty elements after in the struct.
	Value *big.Int `json:"value,omitempty" rlp:"optional"`
}

func (f callFrame) TypeString() string {
	return f.Type.String()
}

func (f callFrame) failed() bool {
	return len(f.Error) > 0
}

func (f *callFrame) processOutput(output []byte, err error) {
	output = common.CopyBytes(output)
	if err == nil {
		f.Output = output
		return
	}
	f.Error = err.Error()
	if f.Type == vm.CREATE || f.Type == vm.CREATE2 {
		f.To = libcommon.Address{}
	}
	if !errors.Is(err, vm.ErrExecutionReverted) || len(output) == 0 {
		return
	}
	f.Output = output
	if len(output) < 4 {
		return
	}
	if unpacked, err := abi.UnpackRevert(output); err == nil {
		f.Revertal = unpacked
	}
}

type callFrameMarshaling struct {
	TypeString string `json:"type"`
	Gas        hexutil.Uint64
	GasUsed    hexutil.Uint64
	Value      *hexutil.Big
	Input      hexutility.Bytes
	Output     hexutility.Bytes
}

type callTracer struct {
	noopTracer
	callstack        []callFrame
	config           callTracerConfig
	gasLimit         uint64
	interrupt        uint32 // Atomic flag to signal execution interruption
	reason           error  // Textual reason for the interruption
	logIndex         uint64
	failedLogIndexes []uint64
}

type callTracerConfig struct {
	OnlyTopCall bool `json:"onlyTopCall"` // If true, call tracer won't collect any subcalls
	WithLog     bool `json:"withLog"`     // If true, call tracer will collect event logs
}

// newCallTracer returns a native go tracer which tracks
// call frames of a tx, and implements vm.EVMLogger.
func newCallTracer(ctx *tracers.Context, cfg json.RawMessage) (tracers.Tracer, error) {
	var config callTracerConfig
	if cfg != nil {
		if err := json.Unmarshal(cfg, &config); err != nil {
			return nil, err
		}
	}
	// First callframe contains tx context info
	// and is populated on start and end.
	return &callTracer{callstack: make([]callFrame, 1), config: config}, nil
}

// CaptureStart implements the EVMLogger interface to initialize the tracing operation.
func (t *callTracer) CaptureStart(env vm.VMInterface, from libcommon.Address, to libcommon.Address, precompile, create bool, input []byte, gas uint64, value *uint256.Int, code []byte) {
	t.callstack[0] = callFrame{
		Type:  vm.CALL,
		From:  from,
		To:    to,
		Input: common.CopyBytes(input),
		Gas:   gas,
	}
	if value != nil {
		t.callstack[0].Value = value.ToBig()
	}
	if create {
		t.callstack[0].Type = vm.CREATE
	}
}

// CaptureEnd is called after the call finishes to finalize the tracing.
func (t *callTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	t.callstack[0].processOutput(output, err)
}

// CaptureState implements the EVMLogger interface to trace a single step of VM execution.
func (t *callTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	// Only logs need to be captured via opcode processing
	if !t.config.WithLog {
		return
	}
	// Avoid processing nested calls when only caring about top call
	if t.config.OnlyTopCall && depth > 0 {
		return
	}
	// Skip if tracing was interrupted
	if atomic.LoadUint32(&t.interrupt) > 0 {
		return
	}
	switch op {
	case vm.LOG0, vm.LOG1, vm.LOG2, vm.LOG3, vm.LOG4:
		size := int(op - vm.LOG0)

		stack := scope.Stack
		stackData := stack.Data

		// Don't modify the stack
		mStart := stackData[len(stackData)-1]
		mSize := stackData[len(stackData)-2]
		topics := make([]libcommon.Hash, size)
		for i := 0; i < size; i++ {
			topic := stackData[len(stackData)-2-(i+1)]
			topics[i] = libcommon.Hash(topic.Bytes32())
		}

		data := scope.Memory.GetCopy(int64(mStart.Uint64()), int64(mSize.Uint64()))
		log := callLog{Address: scope.Contract.Address(), Topics: topics, Data: hexutility.Bytes(data), Index: t.logIndex}
		t.logIndex++
		t.callstack[len(t.callstack)-1].Logs = append(t.callstack[len(t.callstack)-1].Logs, log)
	}
}

// CaptureEnter is called when EVM enters a new scope (via call, create or selfdestruct).
func (t *callTracer) CaptureEnter(typ vm.OpCode, from libcommon.Address, to libcommon.Address, precompile, create bool, input []byte, gas uint64, value *uint256.Int, code []byte) {
	if t.config.OnlyTopCall {
		return
	}
	// Skip if tracing was interrupted
	if atomic.LoadUint32(&t.interrupt) > 0 {
		return
	}

	call := callFrame{
		Type:  typ,
		From:  from,
		To:    to,
		Input: common.CopyBytes(input),
		Gas:   gas,
	}
	if value != nil {
		call.Value = value.ToBig()
	} else {
		if typ == vm.DELEGATECALL {
			call.Value = t.callstack[len(t.callstack)-1].Value
		}
	}
	t.callstack = append(t.callstack, call)
}

// CaptureExit is called when EVM exits a scope, even if the scope didn't
// execute any code.
func (t *callTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	if t.config.OnlyTopCall {
		return
	}
	size := len(t.callstack)
	if size <= 1 {
		return
	}
	// pop call
	call := t.callstack[size-1]
	t.callstack = t.callstack[:size-1]
	size -= 1

	call.GasUsed = gasUsed
	call.processOutput(output, err)
	t.callstack[size-1].Calls = append(t.callstack[size-1].Calls, call)
}

func (t *callTracer) CaptureTxStart(gasLimit uint64) {
	t.gasLimit = gasLimit
	t.logIndex = 0
	t.failedLogIndexes = make([]uint64, 0)
}

func (t *callTracer) CaptureTxEnd(restGas uint64) {
	t.callstack[0].GasUsed = t.gasLimit - restGas
	if t.config.WithLog {
		// Logs are not emitted when the call fails
		clearFailedLogs(&t.callstack[0], false, &t.failedLogIndexes)
		fixLogIndexGap(&t.callstack[0], &t.failedLogIndexes)
	}
	t.logIndex = 0
	t.failedLogIndexes = nil
}

// GetResult returns the json-encoded nested list of call traces, and any
// error arising from the encoding or forceful termination (via `Stop`).
func (t *callTracer) GetResult() (json.RawMessage, error) {
	if len(t.callstack) != 1 {
		return nil, errors.New("incorrect number of top-level calls")
	}
	res, err := json.Marshal(t.callstack[0])
	if err != nil {
		return nil, err
	}
	return json.RawMessage(res), t.reason
}

// Stop terminates execution of the tracer at the first opportune moment.
func (t *callTracer) Stop(err error) {
	t.reason = err
	atomic.StoreUint32(&t.interrupt, 1)
}

// clearFailedLogs clears the logs of a callframe and all its children
// in case of execution failure.
func clearFailedLogs(cf *callFrame, parentFailed bool, failedLogIndexes *[]uint64) {
	failed := cf.failed() || parentFailed
	// Clear own logs
	if failed {
		if len(cf.Logs) > 0 {
			for _, log := range cf.Logs {
				*failedLogIndexes = append(*failedLogIndexes, log.Index)
			}
		}
		cf.Logs = nil
	}
	for i := range cf.Calls {
		clearFailedLogs(&cf.Calls[i], failed, failedLogIndexes)
	}
}

// re-index logs in callFrame
func fixLogIndexGap(cf *callFrame, failedLogIndexes *[]uint64) {
	if len(cf.Logs) > 0 {
		for i := range cf.Logs {
			currentLogOriginalIndex := cf.Logs[i].Index
			for _, deletedIndex := range *failedLogIndexes {
				if currentLogOriginalIndex > deletedIndex {
					cf.Logs[i].Index--
				}
			}
		}
	}
	for i := range cf.Calls {
		fixLogIndexGap(&cf.Calls[i], failedLogIndexes)
	}
}
