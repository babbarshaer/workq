package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/iamduo/workq/int/captain"
	"github.com/iamduo/workq/int/cmdlog"
	"github.com/iamduo/workq/int/handlers"
	"github.com/iamduo/workq/int/job"
	"github.com/iamduo/workq/int/prot"
	"github.com/iamduo/workq/int/server"
)

var logo = ". . .,---.\n" +
	"| | ||   |\n" +
	"`-'-'`---|\n" +
	"         |\n"

func main() {
	m := NewMain()
	if err := m.Run(os.Args[1:]); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// Main represents the main execution.
// Eventually all in/out will be scoped within here.
type Main struct{}

func NewMain() *Main {
	return &Main{}
}

func (m *Main) Run(args []string) error {
	fmt.Printf("\n%s", logo)

	flagSet := flag.NewFlagSet("wq", flag.ExitOnError)
	listen := flagSet.String("listen", "127.0.0.1:9922", "Listen on HOST:PORT, default 127.0.0.1:9922")
	cmdLogPath := flagSet.String("cmdlog-path", "", "Path to command log directory")
	segSize := flagSet.Uint("cmdlog-seg-size", 67108864, "Minimum segment file size in bytes, defaults to 64MiB")
	syncPolicy := flagSet.String("cmdlog-sync", "interval", "Disk sync policy (interval,os,always), defaults to syncing at an interval")
	syncInterval := flagSet.Uint("cmdlog-sync-int", 1000, "Disk sync interval in milliseconds")
	cleanInterval := flagSet.Uint("cmdlog-clean-int", 300000, "Cleaning interval in milliseconds")
	flagSet.Parse(args)

	var s *server.Server
	reg := job.NewRegistry()
	queueController := job.NewQueueController()
	jobController := job.NewController(reg, queueController, &job.Usage{})

	if *cmdLogPath != "" {
		stream := captain.NewStream(*cmdLogPath, cmdlog.MagicHeader)
		appOpts, err := buildAppenderOptions(*segSize, *syncPolicy, *syncInterval)
		if err != nil {
			return err
		}

		cursor, err := stream.OpenCursor()
		if err != nil {
			return err
		}

		lockTimeout := 1 * time.Second

		if err := captain.TimeoutLock(cursor.Lock, lockTimeout); err != nil {
			if err == captain.ErrLockTimeout {
				return errors.New("Timeout waiting for cmdlog lock (cursor)")
			}

			return err
		}

		err = cmdlog.Replay(cursor, jobController)
		if err != nil {
			return fmt.Errorf("Replay Error: %s", err.Error())
		}

		streamCleaner, err := stream.OpenCleaner()
		if err != nil {
			return err
		}

		cursor.Reset()
		cmdCleaner, err := cmdlog.NewWarmedCommandCleaner(reg, cursor)
		if err != nil {
			return err
		}
		cursor.Unlock()

		if err = cmdlog.StartCleaningCycle(streamCleaner, cmdCleaner.Clean, *cleanInterval); err != nil {
			return fmt.Errorf("Cleaning Error: %s", err.Error())
		}

		appender, err := stream.OpenAppender(appOpts)
		if err != nil {
			return err
		}

		if err = captain.TimeoutLock(appender.Lock, lockTimeout); err != nil {
			if err == captain.ErrLockTimeout {
				return errors.New("Timeout waiting for cmdlog lock (appender)")
			}

			return err
		}
		defer appender.Unlock()

		breaker := &cmdlog.CircuitBreaker{}
		handlers := buildHandlers(
			reg,
			queueController,
			cmdlog.NewControllerProxy(cmdlog.NewCircuitBreakerAppender(breaker, appender), jobController),
		)
		router := cmdlog.NewCircuitBreakerRouter(breaker, buildRouter(handlers))
		s = buildServer(*listen, router)
	} else {
		handlers := buildHandlers(reg, queueController, jobController)
		router := buildRouter(handlers)
		s = buildServer(*listen, router)
	}

	fmt.Printf("Listening on %s\n", *listen)
	return s.ListenAndServe()
}

func buildServer(listen string, router server.Router) *server.Server {
	return server.New(listen, router, prot.Prot{}, buildServerUsage())
}

func buildRouter(hldrs map[string]server.Handler) *server.CmdRouter {
	return &server.CmdRouter{Handlers: hldrs, UnknownHandler: &handlers.UnknownHandler{}}
}

func buildHandlers(reg *job.Registry, qc job.QueueControllerInterface, jc job.ControllerInterface) map[string]server.Handler {
	return map[string]server.Handler{
		prot.CmdAdd:      handlers.NewAddHandler(jc),
		prot.CmdRun:      handlers.NewRunHandler(jc),
		prot.CmdSchedule: handlers.NewScheduleHandler(jc),
		prot.CmdDelete:   handlers.NewDeleteHandler(jc),
		prot.CmdLease:    handlers.NewLeaseHandler(jc),
		prot.CmdComplete: handlers.NewCompleteHandler(jc),
		prot.CmdFail:     handlers.NewFailHandler(jc),
		prot.CmdResult:   handlers.NewResultHandler(reg, qc),
		prot.CmdInspect: handlers.NewInspectHandler(
			handlers.NewInspectServerHandler(buildServerUsage(), &handlers.Usage{}),
			handlers.NewInspectQueuesHandler(qc),
			handlers.NewInspectQueueHandler(qc),
			handlers.NewInspectJobsHandler(reg, qc),
			handlers.NewInspectJobHandler(reg),
		),
	}
}

var serverUsage *server.Usage

func buildServerUsage() *server.Usage {
	if serverUsage == nil {
		serverUsage = new(server.Usage)
	}

	return serverUsage
}

func buildAppenderOptions(size uint, policy string, interval uint) (*captain.AppendOptions, error) {
	var opt = new(captain.AppendOptions)
	opt.SegmentSize = size

	switch policy {
	case "interval":
		opt.SyncPolicy = captain.SyncInterval
	case "os":
		opt.SyncPolicy = captain.SyncOS
	case "always":
		opt.SyncPolicy = captain.SyncAlways
	default:
		return nil, fmt.Errorf("Invalid cmdlog-sync policy: %s", policy)
	}

	if opt.SyncPolicy == captain.SyncInterval && interval == 0 {
		return nil, errors.New("Invalid sync interval, must be greater than 0")
	}

	opt.SyncInterval = interval
	return opt, nil
}
