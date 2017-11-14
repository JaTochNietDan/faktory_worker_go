package faktory_worker

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/contribsys/faktory"
)

type eventType int

const (
	Startup  eventType = 1
	Quiet    eventType = 2
	Shutdown eventType = 3
)

// Register registers a handler for the given jobtype.  It is expected that all jobtypes
// are registered upon process startup.
//
// faktory_worker.Register("ImportantJob", ImportantFunc)
func (mgr *Manager) Register(name string, fn Perform) {
	mgr.jobHandlers[name] = fn
}

// Manager coordinates the processes for the worker.  It is responsible for
// starting and stopping goroutines to perform work at the desired concurrency level
type Manager struct {
	Concurrency int
	Queues      []string
	Pool

	quiet bool
	// The done channel will always block unless
	// the system is shutting down.
	done           chan interface{}
	shutdownWaiter *sync.WaitGroup
	jobHandlers    map[string]Perform
	eventHandlers  map[eventType][]func()
}

// Register a callback to be fired when a process lifecycle event occurs.
// These are useful for hooking into process startup or shutdown.
func (mgr *Manager) On(event eventType, fn func()) {
	mgr.eventHandlers[event] = append(mgr.eventHandlers[event], fn)
}

// After calling Quiet(), no more jobs will be pulled
// from Faktory by this process.
func (mgr *Manager) Quiet() {
	log.Println("Quieting...")
	mgr.quiet = true
	mgr.fireEvent(Quiet)
}

// Terminate signals that the various components should shutdown.
// Blocks on the shutdownWaiter until all components have finished.
func (mgr *Manager) Terminate() {
	log.Println("Shutting down...")
	close(mgr.done)
	mgr.fireEvent(Shutdown)
	mgr.shutdownWaiter.Wait()
	mgr.Pool.Close()
	log.Println("Goodbye")
	os.Exit(0)
}

// NewManager returns a new manager with default values.
func NewManager() *Manager {
	return &Manager{
		Concurrency: 20,
		Queues:      []string{"default"},

		done:           make(chan interface{}),
		shutdownWaiter: &sync.WaitGroup{},
		jobHandlers:    map[string]Perform{},
		eventHandlers: map[eventType][]func(){
			Startup:  []func(){},
			Quiet:    []func(){},
			Shutdown: []func(){},
		},
	}
}

// Run starts processing jobs.
// This method does not return.
func (mgr *Manager) Run() {
	// This will signal to Faktory that all connections from this process
	// are worker connections.
	faktory.RandomProcessWid = strconv.FormatInt(rand.Int63(), 32)

	if mgr.Pool == nil {
		pool, err := NewChannelPool(0, mgr.Concurrency, func() (Closeable, error) { return faktory.Open() })
		if err != nil {
			panic(err)
		}
		mgr.Pool = pool
	}

	mgr.fireEvent(Startup)

	go heartbeat(mgr)

	for i := 0; i < mgr.Concurrency; i++ {
		go process(mgr, i)
	}

	sigchan := hookSignals()

	for {
		sig := <-sigchan
		handleSignal(sig, mgr)
	}
}

func heartbeat(mgr *Manager) {
	mgr.shutdownWaiter.Add(1)
	timer := time.NewTicker(5 * time.Second)
	for {
		select {
		case <-timer.C:
			// we don't care about errors, assume any network
			// errors will heal eventually
			_ = mgr.with(func(c *faktory.Client) error {
				sig, err := c.Beat()
				if sig != "" {
					if sig == "terminate" {
						handleSignal(SIGTERM, mgr)
					} else if sig == "quiet" {
						handleSignal(SIGTSTP, mgr)
					}
				}
				return err
			})
		case <-mgr.done:
			timer.Stop()
			mgr.shutdownWaiter.Done()
			return
		}
	}
}

func handleSignal(sig os.Signal, mgr *Manager) {
	switch sig {
	case SIGTERM:
		go func() {
			mgr.Terminate()
		}()
	case SIGINT:
		go func() {
			mgr.Terminate()
		}()
	case SIGTSTP:
		go func() {
			mgr.Quiet()
		}()
	}
}

func process(mgr *Manager, idx int) {
	mgr.shutdownWaiter.Add(1)
	// delay initial fetch randomly to prevent thundering herd.
	time.Sleep(time.Duration(rand.Int31()))
	defer mgr.shutdownWaiter.Done()

	for {
		if mgr.quiet {
			return
		}

		// fetch job
		var job *faktory.Job
		var err error

		err = mgr.with(func(c *faktory.Client) error {
			job, err = c.Fetch(mgr.Queues...)
			if err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			fmt.Println(err)
			time.Sleep(1 * time.Second)
			continue
		}

		// execute
		if job != nil {
			perform := mgr.jobHandlers[job.Type]
			if perform == nil {
				mgr.with(func(c *faktory.Client) error {
					return c.Fail(job.Jid, fmt.Errorf("No handler for %s", job.Type), nil)
				})
			} else {
				err := perform(ctxFor(job), job.Args...)
				mgr.with(func(c *faktory.Client) error {
					if err != nil {
						return c.Fail(job.Jid, err, nil)
					} else {
						return c.Ack(job.Jid)
					}
				})
			}
		} else {
			// if there are no jobs, Faktory will block us on
			// the first queue, so no need to poll or sleep
		}

		// check for shutdown
		select {
		case <-mgr.done:
			return
		default:
		}

	}
}

func (mgr *Manager) fireEvent(event eventType) {
	for _, fn := range mgr.eventHandlers[event] {
		fn()
	}
}

// DefaultContext embeds Go's standard context and associates it with a job ID.
type DefaultContext struct {
	context.Context

	JID string
}

// Jid returns the job ID for the default context
func (c *DefaultContext) Jid() string {
	return c.JID
}

func ctxFor(job *faktory.Job) Context {
	return &DefaultContext{
		Context: context.Background(),
		JID:     job.Jid,
	}
}

func (mgr *Manager) with(fn func(fky *faktory.Client) error) error {
	conn, err := mgr.Pool.Get()
	if err != nil {
		return err
	}
	pc := conn.(*PoolConn)
	f, ok := pc.Closeable.(*faktory.Client)
	if !ok {
		return fmt.Errorf("Connection is not a Faktory client instance: %+v", conn)
	}
	err = fn(f)
	conn.Close()
	return err
}
