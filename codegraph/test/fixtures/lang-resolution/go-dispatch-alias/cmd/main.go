package main

import (
	"github.com/example/godispatch/internal/health"
	"github.com/example/godispatch/internal/jobs"
	pinger "github.com/example/godispatch/internal/pinger2"
	"github.com/example/godispatch/internal/runner"
)

func main() {
	_ = pinger.GetURL()
	_ = health.GetURL()

	r := runner.New()
	r.Register(jobs.NewCleanup())
}
