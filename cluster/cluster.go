package cluster

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/10gen/mongo-go-driver/internal"
	"github.com/10gen/mongo-go-driver/server"
)

// New creates a new cluster. Internally, it
// creates a new Monitor with which to monitor the
// state of the cluster. When the Cluster is closed,
// the monitor will be stopped.
func New(opts ...Option) (*Cluster, error) {
	monitor, err := StartMonitor(opts...)
	if err != nil {
		return nil, err
	}

	cluster := &Cluster{
		monitor:     monitor,
		ownsMonitor: true,
		waiters:     make(map[int64]chan struct{}),
		rand:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	cluster.subscribeToMonitor()
	return cluster, nil
}

// NewWithMonitor creates a new Cluster from
// an existing monitor. When the cluster is closed,
// the monitor will not be stopped.
func NewWithMonitor(monitor *Monitor) *Cluster {
	cluster := &Cluster{
		monitor: monitor,
		waiters: make(map[int64]chan struct{}),
		rand:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	cluster.subscribeToMonitor()
	return cluster
}

// ServerSelector is a function that selects a server.
type ServerSelector func(*Desc, []*server.Desc) ([]*server.Desc, error)

// Cluster represents a logical connection to a cluster.
type Cluster struct {
	monitor      *Monitor
	ownsMonitor  bool
	waiters      map[int64]chan struct{}
	lastWaiterID int64
	waiterLock   sync.Mutex
	desc         *Desc
	descLock     sync.Mutex
	rand         *rand.Rand
}

// Close closes the cluster.
func (c *Cluster) Close() {
	if c.ownsMonitor {
		c.monitor.Stop()
	}
}

// Desc gets a description of the cluster.
func (c *Cluster) Desc() *Desc {
	var desc *Desc
	c.descLock.Lock()
	desc = c.desc
	c.descLock.Unlock()
	return desc
}

// RequestImmediateCheck will send heartbeats to all the servers in the
// cluster right away, instead of waiting for the heartbeat timeout.
func (c *Cluster) RequestImmediateCheck() {
	c.monitor.RequestImmediateCheck()
}

// SelectServer selects a server given a selector.
func (c *Cluster) SelectServer(ctx context.Context, selector ServerSelector) (Server, error) {
	timer := time.NewTimer(c.monitor.serverSelectionTimeout)
	updated, id := c.awaitUpdates()
	for {
		clusterDesc := c.Desc()

		suitable, err := selector(clusterDesc, clusterDesc.Servers)
		if err != nil {
			return nil, err
		}

		if len(suitable) > 0 {
			timer.Stop()
			c.removeWaiter(id)
			selected := suitable[c.rand.Intn(len(suitable))]

			// TODO: put this logic into the monitor...
			c.monitor.serversLock.Lock()
			server := c.monitor.servers[selected.Endpoint]
			c.monitor.serversLock.Unlock()
			return server, nil
		}

		c.monitor.RequestImmediateCheck()

		select {
		case <-ctx.Done():
			timer.Stop()
			c.removeWaiter(id)
			return nil, internal.WrapError(ctx.Err(), "server selection failed")
		case <-updated:
			// topology has changed
		case <-timer.C:
			c.removeWaiter(id)
			return nil, errors.New("server selection timed out")
		}
	}
}

// awaitUpdates returns a channel which will be signaled when the
// cluster description is updated, and an id which can later be used
// to remove this channel from the clusterImpl.waiters map.
func (c *Cluster) awaitUpdates() (<-chan struct{}, int64) {
	id := atomic.AddInt64(&c.lastWaiterID, 1)
	ch := make(chan struct{}, 1)
	c.waiterLock.Lock()
	c.waiters[id] = ch
	c.waiterLock.Unlock()
	return ch, id
}

func (c *Cluster) removeWaiter(id int64) {
	c.waiterLock.Lock()
	_, found := c.waiters[id]
	if !found {
		panic("Could not find channel with provided id to remove")
	}
	delete(c.waiters, id)
	c.waiterLock.Unlock()
}

func (c *Cluster) subscribeToMonitor() {
	updates, _, _ := c.monitor.Subscribe()
	go func() {
		for desc := range updates {
			c.descLock.Lock()
			c.desc = desc
			c.descLock.Unlock()

			c.waiterLock.Lock()
			for _, waiter := range c.waiters {
				select {
				case waiter <- struct{}{}:
				default:
				}
			}
			c.waiterLock.Unlock()
		}
		c.waiterLock.Lock()
		for id, ch := range c.waiters {
			close(ch)
			delete(c.waiters, id)
		}
		c.waiterLock.Unlock()
	}()
}