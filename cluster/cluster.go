/*
 * Copyright (c) 2018 Miguel Ángel Ortuño.
 * See the LICENSE file for more information.
 */

package cluster

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ortuman/jackal/log"
	"github.com/ortuman/jackal/runqueue"
	"github.com/ortuman/jackal/xmpp"
	"github.com/ortuman/jackal/xmpp/jid"
)

var createMemberList = func(localName string, bindPort int, timeout time.Duration, cluster *Cluster) (memberList, error) {
	return newDefaultMemberList(localName, bindPort, timeout, cluster)
}

// Metadata type represents all metadata information associated to a node.
type Metadata struct {
	Version   string
	GoVersion string
}

// Node represents a concrete c2s node and metadata information.
type Node struct {
	Name     string
	Metadata Metadata
}

// Delegate is the interface that will receive all c2s related events.
type Delegate interface {
	NodeJoined(ctx context.Context, node *Node)
	NodeUpdated(ctx context.Context, node *Node)
	NodeLeft(ctx context.Context, node *Node)

	NotifyMessage(ctx context.Context, msg *Message)
}

// memberList interface defines the common c2s member list methods.
type memberList interface {
	Members() []Node

	Join(hosts []string) error
	Shutdown() error

	SendReliable(node string, msg []byte) error
}

// Cluster represents a c2s sub system.
type Cluster struct {
	cfg        *Config
	buf        *bytes.Buffer
	delegate   Delegate
	memberList memberList
	membersMu  sync.RWMutex
	members    map[string]*Node
	runQueue   *runqueue.RunQueue
}

// New returns an initialized c2s instance
func New(config *Config, delegate Delegate) (*Cluster, error) {
	if config == nil {
		return nil, nil
	}
	c := &Cluster{
		cfg:      config,
		delegate: delegate,
		buf:      bytes.NewBuffer(nil),
		members:  make(map[string]*Node),
		runQueue: runqueue.New("cluster"),
	}
	ml, err := createMemberList(config.Name, config.BindPort, config.InTimeout, c)
	if err != nil {
		return nil, err
	}
	c.memberList = ml
	return c, nil
}

// Join tries to join the c2s by contacting all the given hosts.
func (c *Cluster) Join() error {
	log.Infof("local node: %s", c.LocalNode())

	c.membersMu.Lock()
	for _, m := range c.memberList.Members() {
		if m.Name == c.LocalNode() {
			continue
		}
		log.Infof("registered cluster node: %s", m.Name)
		c.members[m.Name] = &m
	}
	c.membersMu.Unlock()
	return c.memberList.Join(c.cfg.Hosts)
}

// LocalNode returns the local node identifier.
func (c *Cluster) LocalNode() string {
	return c.cfg.Name
}

// C2SStream returns a cluster C2S stream.
func (c *Cluster) C2SStream(jid *jid.JID, presence *xmpp.Presence, context map[string]interface{}, node string) *C2S {
	return newC2S(uuid.New().String(), jid, presence, context, node, c)
}

// SendMessageTo sends a cluster message to a concrete node.
func (c *Cluster) SendMessageTo(ctx context.Context, node string, msg *Message) {
	c.runQueue.Run(func() {
		if err := c.send(ctx, msg, node); err != nil {
			log.Error(err)
			return
		}
	})
}

// BroadcastMessage broadcasts a cluster message to all nodes.
func (c *Cluster) BroadcastMessage(ctx context.Context, msg *Message) {
	c.runQueue.Run(func() {
		if err := c.broadcast(ctx, msg); err != nil {
			log.Error(err)
		}
	})
}

// Shutdown shuts down cluster sub system.
func (c *Cluster) Shutdown() error {
	errCh := make(chan error, 1)
	c.runQueue.Stop(func() {
		errCh <- c.memberList.Shutdown()
	})
	return <-errCh
}

func (c *Cluster) send(_ context.Context, msg *Message, toNode string) error {
	return c.memberList.SendReliable(toNode, c.encodeMessage(msg))
}

func (c *Cluster) broadcast(_ context.Context, msg *Message) error {
	msgBytes := c.encodeMessage(msg)

	c.membersMu.RLock()
	defer c.membersMu.RUnlock()

	var errs []error
	var errsMu sync.Mutex

	var wg sync.WaitGroup
	for _, node := range c.members {
		wg.Add(1)
		go func(node string, b []byte) {
			defer wg.Done()

			if node == c.LocalNode() {
				return
			}
			if err := c.memberList.SendReliable(node, b); err != nil {
				errsMu.Lock()
				errs = append(errs, err)
				errsMu.Unlock()
			}
		}(node.Name, msgBytes)
	}
	wg.Wait()

	if len(errs) > 0 {
		var sb strings.Builder
		for i, err := range errs {
			if i != 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(err.Error())
		}
		return errors.New(sb.String())
	}
	return nil
}

func (c *Cluster) handleNotifyJoin(ctx context.Context, n *Node) {
	if n.Name == c.LocalNode() {
		return
	}
	c.membersMu.Lock()
	c.members[n.Name] = n
	c.membersMu.Unlock()

	log.Infof("registered cluster node: %s", n.Name)
	if c.delegate != nil && n.Name != c.LocalNode() {
		c.delegate.NodeJoined(ctx, n)
	}
}

func (c *Cluster) handleNotifyUpdate(ctx context.Context, n *Node) {
	if n.Name == c.LocalNode() {
		return
	}
	c.membersMu.Lock()
	c.members[n.Name] = n
	c.membersMu.Unlock()

	log.Infof("updated cluster node: %s", n.Name)
	if c.delegate != nil && n.Name != c.LocalNode() {
		c.delegate.NodeUpdated(ctx, n)
	}
}

func (c *Cluster) handleNotifyLeave(ctx context.Context, n *Node) {
	if n.Name == c.LocalNode() {
		return
	}
	c.membersMu.Lock()
	delete(c.members, n.Name)
	c.membersMu.Unlock()

	log.Infof("unregistered cluster node: %s", n.Name)
	if c.delegate != nil && n.Name != c.LocalNode() {
		c.delegate.NodeLeft(ctx, n)
	}
}

func (c *Cluster) handleNotifyMsg(ctx context.Context, msg []byte) {
	if len(msg) == 0 {
		return
	}
	var m Message
	buf := bytes.NewBuffer(msg)
	if err := m.FromBytes(buf); err != nil {
		log.Error(err)
		return
	}
	if c.delegate != nil {
		c.delegate.NotifyMessage(ctx, &m)
	}
}

func (c *Cluster) encodeMessage(msg *Message) []byte {
	defer c.buf.Reset()

	_ = msg.ToBytes(c.buf)
	msgBytes := make([]byte, c.buf.Len(), c.buf.Len())
	copy(msgBytes, c.buf.Bytes())
	return msgBytes
}
