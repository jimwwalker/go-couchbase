package couchbase

import (
	"errors"
	"time"

	"github.com/dustin/gomemcached/client"
)

// Error raised when a connection can't be retrieved from a pool.
var TimeoutError = errors.New("timeout waiting to build connection")
var closedPool = errors.New("the pool is closed")

// Default timeout for retrieving a connection from the pool.
var ConnPoolTimeout = time.Hour * 24 * 30

type connectionPool struct {
	host        string
	mkConn      func(host string, ah AuthHandler) (*memcached.Client, error)
	auth        AuthHandler
	connections chan *memcached.Client
	createsem   chan bool
}

func newConnectionPool(host string, ah AuthHandler, poolSize, poolOverflow int) *connectionPool {
	return &connectionPool{
		host:        host,
		connections: make(chan *memcached.Client, poolSize),
		createsem:   make(chan bool, poolSize+poolOverflow),
		mkConn:      defaultMkConn,
		auth:        ah,
	}
}

func defaultMkConn(host string, ah AuthHandler) (*memcached.Client, error) {
	conn, err := memcached.Connect("tcp", host)
	if err != nil {
		return nil, err
	}
	name, pass := ah.GetCredentials()
	if name != "default" {
		_, err = conn.Auth(name, pass)
		if err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func (cp *connectionPool) Close() (err error) {
	defer func() { err, _ = recover().(error) }()
	close(cp.connections)
	for c := range cp.connections {
		c.Close()
	}
	return
}

func (cp *connectionPool) GetWithTimeout(d time.Duration) (*memcached.Client, error) {
	if cp == nil {
		return nil, errors.New("no pool")
	}

	t := time.NewTimer(time.Millisecond)
	defer t.Stop()

	select {
	case rv, isopen := <-cp.connections:
		if !isopen {
			return nil, closedPool
		}
		return rv, nil
	case <-t.C:
		t.Reset(d) // Reuse the timer for the full timeout.
		select {
		case rv, isopen := <-cp.connections:
			if !isopen {
				return nil, closedPool
			}
			return rv, nil
		case cp.createsem <- true:
			// Build a connection if we can't get a real one.
			// This can potentially be an overflow connection, or
			// a pooled connection.
			rv, err := cp.mkConn(cp.host, cp.auth)
			if err != nil {
				// On error, release our create hold
				<-cp.createsem
			}
			return rv, err
		case <-t.C:
			return nil, TimeoutError
		}
	}
}

func (cp *connectionPool) Get() (*memcached.Client, error) {
	return cp.GetWithTimeout(ConnPoolTimeout)
}

func (cp *connectionPool) Return(c *memcached.Client) {
	if cp == nil || c == nil {
		return
	}

	if c.IsHealthy() {
		defer func() {
			if recover() != nil {
				// This happens when the pool has already been
				// closed and we're trying to return a
				// connection to it anyway.  Just close the
				// connection.
				c.Close()
			}
		}()

		select {
		case cp.connections <- c:
		default:
			// Overflow connection.
			<-cp.createsem
			c.Close()
		}
	} else {
		<-cp.createsem
		c.Close()
	}
}

func (cp *connectionPool) StartTapFeed(args *memcached.TapArguments) (*memcached.TapFeed, error) {
	if cp == nil {
		return nil, errors.New("no pool")
	}
	mc, err := cp.Get()
	if err != nil {
		return nil, err
	}

	// A connection can't be used after TAP; Dont' count it against the
	// connection pool capacity
	<-cp.createsem

	return mc.StartTapFeed(*args)
}
