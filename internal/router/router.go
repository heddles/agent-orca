package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/heddles/agent-orca/internal/executor"
	"github.com/heddles/agent-orca/internal/mcp"
	"github.com/heddles/agent-orca/internal/state"
	tiktoken "github.com/pkoukk/tiktoken-go"
	hindsight "github.com/vectorize-io/hindsight/hindsight-clients/go"
)

var defaultEncoder *tiktoken.Tiktoken

func init() {
	var err error
	defaultEncoder, err = tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		defaultEncoder = nil
	}
}

type continuationKey struct{}

// Router is the core model-router sidecar.
type Router struct {
	cfg               *Config
	auth              *Authenticator
	ruleRouter        *RuleRouter
	metaRouter        *MetaRouter
	store             state.Store
	priorMessages     []Message
	messages          []Message
	mu                sync.Mutex
	spendUSD          float64
	checkpointTTL     time.Duration
	handedOff         bool
	waitingForInput   bool
	doneExplicit      bool
	failedExplicit    bool
	resumedWithAnswer bool
	liveBufferTokens  int
	compacting        bool
	compactDone       chan struct{}
	tokens            *TokenBroadcaster
	mcpClient         *mcp.Client
	metrics           *Metrics
	guardrails        *GuardrailPipeline
	consecutiveNoops  int
	toolCallCounts    map[string]int
	toolTimeout       time.Duration
	toolCallSigs      map[string]int
	loopDetected      bool
	cancelCtx         context.Context
	cancelFunc        context.CancelFunc
	exec              *executor.Executor
	streamingActive   atomic.Bool
}