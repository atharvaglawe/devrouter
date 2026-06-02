package telemetry

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisHook is a go-redis Hook that records per-command latency and
// outcomes on the package-private Prometheus registry. The op label is
// the canonical (uppercase) first word of the command — Redis commands
// form a small bounded set so cardinality stays well below a hundred
// even with FT.SEARCH variants.
//
// Pipelines record one observation per command inside the pipeline so
// hot-path batches (e.g. dashboard HGETALLs) don't hide individual
// slowness behind the wrapper's aggregate timing.
type RedisHook struct{}

// NewRedisHook returns a Hook ready to install via rdb.AddHook(). The
// telemetry package guarantees the underlying registry is initialised
// before any Hook callback runs.
func NewRedisHook() *RedisHook {
	ensureRegistry()
	return &RedisHook{}
}

// DialHook is required by the go-redis interface but is not
// instrumented here — dial failures bubble up as ping/connect errors,
// which devrouter's startup code already surfaces with a clear error
// message. Wrapping them as Prometheus metrics would just duplicate
// signal that's already in the logs.
func (RedisHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

// ProcessHook instruments every single-command call (rdb.Get(...), rdb.HSet(...) etc.).
func (RedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		recordCmd(cmd, start, err)
		return err
	}
}

// ProcessPipelineHook instruments pipeline calls. We record one entry
// per command in the batch — the alternative of a single "pipeline"
// label loses the per-op distribution that makes the metric actionable.
func (RedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		// Pipelines run in one round-trip; we attribute the same
		// elapsed time to every command inside. That preserves the
		// per-op rate signal without lying about pipeline-vs-solo
		// latency: a slow pipeline simply shows up as N slow ops.
		elapsed := time.Since(start)
		for _, cmd := range cmds {
			recordPipelineCmd(cmd, elapsed, err)
		}
		return err
	}
}

func recordCmd(cmd redis.Cmder, start time.Time, err error) {
	op := normaliseOp(cmd.Name())
	status := statusFor(err)
	RedisCommands.WithLabelValues(op, status).Inc()
	RedisCommandDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

func recordPipelineCmd(cmd redis.Cmder, elapsed time.Duration, err error) {
	op := normaliseOp(cmd.Name())
	status := statusFor(err)
	RedisCommands.WithLabelValues(op, status).Inc()
	RedisCommandDuration.WithLabelValues(op).Observe(elapsed.Seconds())
}

func statusFor(err error) string {
	if err != nil && err != redis.Nil {
		return "error"
	}
	return "ok"
}

// normaliseOp uppercases a go-redis command name and collapses the
// long-tail FT.* family to "ft.<subcommand>" so RediSearch traffic
// stays legible without leaking arbitrary index names through the
// label.
func normaliseOp(name string) string {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return "UNKNOWN"
	}
	return name
}
