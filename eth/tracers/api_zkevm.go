package tracers

import (
	"encoding/json"

	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/tracers/logger"
	"github.com/ledgerwatch/erigon/turbo/adapter/ethapi"
)

// TraceConfig holds extra parameters to trace functions.
type TraceConfig_ZkEvm struct {
	*logger.LogConfig
	Tracer         *string
	TracerConfig   *json.RawMessage
	Timeout        *string
	Reexec         *uint64
	NoRefunds      *bool // Turns off gas refunds when tracing
	StateOverrides *ethapi.StateOverrides

	BorTraceEnabled *bool
	BorTx           *bool

	CounterCollector *vm.CounterCollector
	SmtDepth         *int
}
