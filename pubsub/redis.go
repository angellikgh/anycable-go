package pubsub

import (
	"errors"
	"github.com/FZambia/sentinel"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/anycable/anycable-go/node"
	"github.com/apex/log"
	"github.com/gomodule/redigo/redis"
)

const (
	maxReconnectAttempts = 5
)

// RedisSubscriber contains information about Redis pubsub connection
type RedisSubscriber struct {
	node             *node.Node
	url              string
	sentinelsEnabled bool
	sentinels        string
	password         string
	masterName       string
	channel          string
	reconnectAttempt int
	log              *log.Entry
}

// NewRedisSubscriber returns new RedisSubscriber struct
func NewRedisSubscriber(node *node.Node, url string, sentinelsEnabled bool, sentinels string, masterName string, password string, channel string) RedisSubscriber {
	return RedisSubscriber{
		node:             node,
		url:              url,
		sentinelsEnabled: sentinelsEnabled,
		sentinels:        sentinels,
		password:         password,
		masterName:       masterName,
		channel:          channel,
		reconnectAttempt: 0,
		log:              log.WithFields(log.Fields{"context": "pubsub"}),
	}
}

// Start connects to Redis and subscribes to the pubsub channel
func (s *RedisSubscriber) Start() error {
	// Check that URL is correct first
	_, err := url.Parse(s.url)

	if err != nil {
		return err
	}

	for {
		if err := s.listen(); err != nil {
			s.log.Warnf("Redis connection failed: %v", err)
		}

		s.reconnectAttempt++

		if s.reconnectAttempt >= maxReconnectAttempts {
			return errors.New("Redis reconnect attempts exceeded")
		}

		delay := nextRetry(s.reconnectAttempt)

		s.log.Infof("Next Redis reconnect attempt in %s", delay)
		time.Sleep(delay)

		s.log.Infof("Reconnecting to Redis...")
	}
}

func (s *RedisSubscriber) listen() error {

	var c redis.Conn
	var err error

	if s.sentinelsEnabled {
		sentinels := strings.Split(s.sentinels, ",")
		sntnl := &sentinel.Sentinel{
			Addrs:      sentinels,
			MasterName: s.masterName,
			Dial: func(addr string) (redis.Conn, error) {
				timeout := 500 * time.Millisecond

				c, err := redis.Dial(
					"tcp",
					addr,
					redis.DialConnectTimeout(timeout),
					redis.DialReadTimeout(timeout),
					redis.DialReadTimeout(timeout),
				)
				if err != nil {
					return nil, err
				}
				return c, nil
			},
		}

		defer sntnl.Close()

		pool := &redis.Pool{
			MaxIdle:     3,
			MaxActive:   64,
			Wait:        true,
			IdleTimeout: 240 * time.Second,
			Dial: func() (redis.Conn, error) {
				masterAddr, err := sntnl.MasterAddr()
				if err != nil {
					return nil, err
				}
				c, err := redis.Dial("tcp", masterAddr, redis.DialPassword(s.password))

				if err != nil {
					return nil, err
				}
				return c, nil
			},
			TestOnBorrow: func(c redis.Conn, t time.Time) error {
				if !sentinel.TestRole(c, "master") {
					return errors.New("Role check failed")
				} else {
					return nil
				}
			},
		}

		defer pool.Close()

		c = pool.Get()

	} else {
		c, err = redis.DialURL(s.url)

		if err != nil {
			return err
		}
	}

	defer c.Close()

	psc := redis.PubSubConn{Conn: c}
	if err := psc.Subscribe(s.channel); err != nil {
		s.log.Errorf("Failed to subscribe to Redis channel: %v", err)
		return err
	}

	s.reconnectAttempt = 0

	done := make(chan error, 1)

	go func() {
		for {
			switch v := psc.Receive().(type) {
			case redis.Message:
				s.log.Debugf("Incoming pubsub message from Redis: %s", v.Data)
				s.node.HandlePubsub(v.Data)
			case redis.Subscription:
				s.log.Infof("Subscribed to Redis channel: %s\n", v.Channel)
			case error:
				s.log.Errorf("Redis subscription error: %v", v)
				done <- v
				break
			}
		}
	}()

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

loop:
	for err == nil {
		select {
		case <-ticker.C:
			if err = psc.Ping(""); err != nil {
				break loop
			}
		case err := <-done:
			// Return error from the receive goroutine.
			return err
		}
	}

	psc.Unsubscribe()
	return <-done
}

func nextRetry(step int) time.Duration {
	secs := (step * step) + (rand.Intn(step*4) * (step + 1))
	return time.Duration(secs) * time.Second
}
