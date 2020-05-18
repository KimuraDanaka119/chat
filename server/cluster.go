package main

import (
	"encoding/gob"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/rpc"
	"sort"
	"sync"
	"time"

	"github.com/tinode/chat/server/auth"
	"github.com/tinode/chat/server/push"
	rh "github.com/tinode/chat/server/ringhash"
	"github.com/tinode/chat/server/store/types"
)

const (
	// Default timeout before attempting to reconnect to a node
	defaultClusterReconnect = 200 * time.Millisecond
	// Number of replicas in ringhash
	clusterHashReplicas = 20
	// Period for running health check on cluster session: terminate sessions with no subscriptions.
	clusterSessionCleanup = 5 * time.Second
)

type clusterNodeConfig struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

type clusterConfig struct {
	// List of all members of the cluster, including this member
	Nodes []clusterNodeConfig `json:"nodes"`
	// Name of this cluster node
	ThisName string `json:"self"`
	// Failover configuration
	Failover *clusterFailoverConfig
}

// ClusterNode is a client's connection to another node.
type ClusterNode struct {
	lock sync.Mutex

	// RPC endpoint
	endpoint *rpc.Client
	// True if the endpoint is believed to be connected
	connected bool
	// True if a go routine is trying to reconnect the node
	reconnecting bool
	// TCP address in the form host:port
	address string
	// Name of the node
	name string
	// Fingerprint of the node: unique value which changes when the node restarts.
	fingerprint int64

	// A number of times this node has failed in a row
	failCount int

	// Channel for shutting down the runner; buffered, 1
	done chan bool
}

// ClusterSess is a basic info on a remote session where the message was created.
type ClusterSess struct {
	// IP address of the client. For long polling this is the IP of the last poll
	RemoteAddr string

	// User agent, a string provived by an authenticated client in {login} packet
	UserAgent string

	// ID of the current user or 0
	Uid types.Uid

	// User's authentication level
	AuthLvl auth.Level

	// Protocol version of the client: ((major & 0xff) << 8) | (minor & 0xff)
	Ver int

	// Human language of the client
	Lang string

	// Device ID
	DeviceID string

	// Device platform: "web", "ios", "android"
	Platform string

	// Session ID
	Sid string
}

// ClusterReq is either a Proxy to Master or Topic Proxy to Topic Master or intra-cluster routing request message.
type ClusterReq struct {
	// Name of the node sending this request
	Node string

	// Ring hash signature of the node sending this request
	// Signature must match the signature of the receiver, otherwise the
	// Cluster is desynchronized.
	Signature string

	// Fingerprint of the node sending this request.
	// Fingerprint changes when the node is restarted.
	Fingerprint int64

	// Client message. Set for C2S requests.
	CliMsg *ClientComMessage
	// Message to be routed. Set for intra-cluster route requests.
	SrvMsg *ServerComMessage
	// Topic message. Set for topic proxy to topic master requests.
	TopicMsg *ProxyTopicMessage

	// Expanded (routable) topic name
	RcptTo string
	// Originating session
	Sess *ClusterSess
	// True if either the original topic (when proxying topics) is gone or
	// the original session (when proxying sessions) has disconnected
	Done bool
}

// ClusterResp is a Master to Proxy response message.
type ClusterResp struct {
	// Server message with the response.
	SrvMsg *ServerComMessage
	// Session ID to forward message to, if any.
	FromSID string
	// Expanded (routable) topic name
	RcptTo string

	// Parameters sent back by the topic master in response a topic proxy request.

	// Original request type.
	OrigRequestType int
	// ID of the affected user.
	Uid types.Uid
	// It is a response to a request from a background session.
	IsBackground bool
}

// ProxyTopicMessage combines topic proxy to master request params.
type ProxyTopicMessage struct {
	// Join (subscribe) request.
	JoinReq *ProxyJoin
	// Broadcast (publish, etc.) request.
	BroadcastReq *ProxyBroadcast
	// Meta request.
	MetaReq *ProxyMeta
	// Leave (unsubscribe) request.
	LeaveReq *ProxyLeave
	// User agent change request.
	UAChangeReq *ProxyUAChange
	// Send deferred notifications request.
	DefrNotifReq *ProxyDeferredNotifications
}

// ProxyJoin contains topic join request parameters.
type ProxyJoin struct {
	// Subscription was in background.
	IsBackground bool
	// User Agent which issued this request.
	UserAgent string
}

// ProxyBroadcast contains topic broadcast request parameters.
type ProxyBroadcast struct{}

// ProxyMeta contains meta (get, sub) request parameters.
type ProxyMeta struct {
	// What is being requested: sub, desc, tags, etc.
	What int
}

// ProxyLeave contains unsubscribe request params.
type ProxyLeave struct {
	// Id of the incoming leave request.
	Id string
	// User ID of the user sent the request.
	UserId types.Uid
	// Leave and unsubscribe.
	Unsub bool
	// Terminate proxy connection to the master topic.
	TerminateProxyConnection bool
}

// ProxyUAChange contains user agent change request params.
type ProxyUAChange struct {
	// User agent string.
	UserAgent string
}

// ProxyDeferredSession represents a background session
// for which we want to send deferred notifications.
type ProxyDeferredSession struct {
	// User this session represents.
	AsUser string
	// Session id.
	Sid string
	// Session user agent.
	UserAgent string
}

// ProxyDeferredNotifications contains a list of sessions
// for which deferred notifications will be sent.
type ProxyDeferredNotifications struct {
	SendNotificationRequests []*ProxyDeferredSession
}

// Proxy request types.
const (
	ProxyRequestJoin      = 1
	ProxyRequestLeave     = 2
	ProxyRequestMeta      = 3
	ProxyRequestBroadcast = 4
)

// Handle outbound node communication: read messages from the channel, forward to remote nodes.
// FIXME(gene): this will drain the outbound queue in case of a failure: all unprocessed messages will be dropped.
// Maybe it's a good thing, maybe not.
func (n *ClusterNode) reconnect() {
	var reconnTicker *time.Ticker

	// Avoid parallel reconnection threads
	n.lock.Lock()
	if n.reconnecting {
		n.lock.Unlock()
		return
	}
	n.reconnecting = true
	n.lock.Unlock()

	var count = 0
	var err error
	for {
		// Attempt to reconnect right away
		if n.endpoint, err = rpc.Dial("tcp", n.address); err == nil {
			if reconnTicker != nil {
				reconnTicker.Stop()
			}
			n.lock.Lock()
			n.connected = true
			n.reconnecting = false
			n.lock.Unlock()
			statsInc("LiveClusterNodes", 1)
			log.Println("cluster: connected to", n.name)
			return
		} else if count == 0 {
			reconnTicker = time.NewTicker(defaultClusterReconnect)
		}

		count++

		select {
		case <-reconnTicker.C:
			// Wait for timer to try to reconnect again. Do nothing if the timer is inactive.
		case <-n.done:
			// Shutting down
			log.Println("cluster: shutdown started at node", n.name)
			reconnTicker.Stop()
			if n.endpoint != nil {
				n.endpoint.Close()
			}
			n.lock.Lock()
			n.connected = false
			n.reconnecting = false
			n.lock.Unlock()
			log.Println("cluster: shut down completed at node", n.name)
			return
		}
	}
}

func (n *ClusterNode) call(proc string, msg, resp interface{}) error {
	if !n.connected {
		return errors.New("cluster: node '" + n.name + "' not connected")
	}

	if err := n.endpoint.Call(proc, msg, resp); err != nil {
		log.Printf("cluster: call failed to '%s' [%s]", n.name, err)

		n.lock.Lock()
		if n.connected {
			n.endpoint.Close()
			n.connected = false
			statsInc("LiveClusterNodes", -1)
			go n.reconnect()
		}
		n.lock.Unlock()
		return err
	}

	return nil
}

func (n *ClusterNode) callAsync(proc string, msg, resp interface{}, done chan *rpc.Call) *rpc.Call {
	if done != nil && cap(done) == 0 {
		log.Panic("cluster: RPC done channel is unbuffered")
	}

	if !n.connected {
		call := &rpc.Call{
			ServiceMethod: proc,
			Args:          msg,
			Reply:         resp,
			Error:         errors.New("cluster: node '" + n.name + "' not connected"),
			Done:          done,
		}
		if done != nil {
			done <- call
		}
		return call
	}

	myDone := make(chan *rpc.Call, 1)
	go func() {
		call := <-myDone
		if call.Error != nil {
			n.lock.Lock()
			if n.connected {
				n.endpoint.Close()
				n.connected = false
				statsInc("LiveClusterNodes", -1)
				go n.reconnect()
			}
			n.lock.Unlock()
		}

		if done != nil {
			done <- call
		}
	}()

	call := n.endpoint.Go(proc, msg, resp, myDone)
	call.Done = done

	return call
}

// Proxy forwards message to master
func (n *ClusterNode) forward(msg *ClusterReq) error {
	msg.Node = globals.cluster.thisNodeName
	var rejected bool
	err := n.call("Cluster.Master", msg, &rejected)
	if err == nil && rejected {
		err = errors.New("cluster: master node out of sync")
	}
	return err
}

// Topic proxy forwards message to topic master
func (n *ClusterNode) forwardToTopicMaster(msg *ClusterReq) error {
	msg.Node = globals.cluster.thisNodeName
	var rejected bool
	err := n.call("Cluster.TopicMaster", msg, &rejected)
	if err == nil && rejected {
		err = errors.New("cluster: topic master node out of sync")
	}
	return err
}

// Master responds to proxy
func (n *ClusterNode) respond(msg *ClusterResp) error {
	var unused bool
	return n.call("Cluster.Proxy", msg, &unused)
}

// Topic master responds to topic proxy
func (n *ClusterNode) respondToTopicProxy(msg *ClusterResp) error {
	var unused bool
	return n.call("Cluster.TopicProxy", msg, &unused)
}

// Routes the message within the cluster.
func (n *ClusterNode) route(msg *ClusterReq) error {
	var unused bool
	return n.call("Cluster.Route", msg, &unused)
}

// Cluster is the representation of the cluster.
type Cluster struct {
	// Cluster nodes with RPC endpoints (excluding current node).
	nodes map[string]*ClusterNode
	// Name of the local node
	thisNodeName string
	// Fingerprint of the local node
	fingerprint int64

	// Resolved address to listed on
	listenOn string

	// Socket for inbound connections
	inbound *net.TCPListener
	// Ring hash for mapping topic names to nodes
	ring *rh.Ring

	// Failover parameters. Could be nil if failover is not enabled
	fo *clusterFailover
}

// Master at topic's master node receives C2S messages from topic's proxy nodes.
// The message is treated like it came from a session: find or create a session locally,
// dispatch the message to it like it came from a normal ws/lp/gRPC connection.
// Called by a remote node.
func (c *Cluster) Master(msg *ClusterReq, rejected *bool) error {
	// Find the local session associated with the given remote session.
	sess := globals.sessionStore.Get(msg.Sess.Sid)

	if msg.Done {
		// Original session has disconnected. Tear down the local proxied session.
		if sess != nil {
			sess.stop <- nil
		}
	} else if msg.Signature == c.ring.Signature() {
		// This cluster member received a request for a topic it owns.
		node := globals.cluster.nodes[msg.Node]

		if node == nil {
			log.Println("cluster: request from an unknown node", msg.Node)
			return nil
		}

		// Check if the remote node has been restarted and if so cleanup stale sessions
		// which originated at that node.
		if node.fingerprint == 0 {
			node.fingerprint = msg.Fingerprint
		} else if node.fingerprint != msg.Fingerprint {
			globals.sessionStore.NodeRestarted(node.name, msg.Fingerprint)
			node.fingerprint = msg.Fingerprint
		}

		if sess == nil {
			// If the session is not found, create it.
			var count int
			sess, count = globals.sessionStore.NewSession(node, msg.Sess.Sid)
			log.Println("cluster: session proxy started", msg.Sess.Sid, count)
			go sess.rpcWriteLoop()
		}

		// Update session params which may have changed since the last call.
		sess.uid = msg.Sess.Uid
		sess.authLvl = msg.Sess.AuthLvl
		sess.ver = msg.Sess.Ver
		sess.userAgent = msg.Sess.UserAgent
		sess.remoteAddr = msg.Sess.RemoteAddr
		sess.lang = msg.Sess.Lang
		sess.deviceID = msg.Sess.DeviceID
		sess.platf = msg.Sess.Platform

		// Dispatch remote message to a local session.
		sess.dispatch(msg.CliMsg)
	} else {
		log.Println("cluster Master: session signature mismatch", msg.Sess.Sid)
		// Reject the request: wrong signature, cluster is out of sync.
		*rejected = true
	}

	return nil
}

// TopicMaster handles requests sent by proxy topic to master topic.
func (c *Cluster) TopicMaster(msg *ClusterReq, rejected *bool) error {
	*rejected = false

	sid := msg.RcptTo + "-" + msg.Node
	sess := globals.sessionStore.Get(sid)
	if msg.Done {
		// Original topic is gone. Tear down the local auxiliary session
		// (the master topic will dissapear as well).
		if sess != nil {
			sess.stop <- nil
		}
		return nil
	}
	if msg.Signature != c.ring.Signature() {
		log.Println("cluster TopicMaster: session signature mismatch", msg.RcptTo)
		*rejected = true
		return nil
	}
	if msg.TopicMsg == nil {
		panic("cluster TopicMaster: nil topic message - topic " + msg.RcptTo)
	}
	node := globals.cluster.nodes[msg.Node]
	if node == nil {
		log.Println("cluster TopicMaster: request from an unknown node", msg.Node)
		return nil
	}
	var origSid string
	if msg.Sess != nil {
		// Sess can be empty for pres messages.
		origSid = msg.Sess.Sid
	}

	// Create a new session if needed.
	if sess == nil {
		// If the session is not found, create it.
		var count int
		sess, count = globals.sessionStore.NewSession(node, sid)

		log.Println("cluster: topic proxy channel started", sid, count)
		go sess.topicProxyWriteLoop(msg.RcptTo)
	}

	switch {
	case msg.TopicMsg.JoinReq != nil:
		sessionJoin := &sessionJoin{
			pkt:          msg.CliMsg,
			sess:         sess,
			isBackground: msg.CliMsg.Sub.Background,
			userAgent:    msg.TopicMsg.JoinReq.UserAgent,
			// Impersonate the original session.
			sessOverrides: &sessionOverrides{
				sid:     origSid,
				origReq: msg.TopicMsg.JoinReq,
			},
		}
		globals.hub.join <- sessionJoin

	case msg.TopicMsg.LeaveReq != nil:
		if t := globals.hub.topicGet(msg.RcptTo); t != nil {
			leave := &sessionLeave{
				id:                       msg.TopicMsg.LeaveReq.Id,
				userId:                   msg.TopicMsg.LeaveReq.UserId,
				unsub:                    msg.TopicMsg.LeaveReq.Unsub,
				terminateProxyConnection: msg.TopicMsg.LeaveReq.TerminateProxyConnection,
				sess:                     sess,
				sessOverrides: &sessionOverrides{
					sid:     origSid,
					origReq: msg.TopicMsg.LeaveReq,
				},
			}
			log.Printf("processing session leave for sid '%s', full req = %+v", origSid, leave)
			t.unreg <- leave
		} else {
			log.Printf("cluster: leave request for unknown topic %s", msg.RcptTo)
		}

	case msg.TopicMsg.MetaReq != nil:
		req := &metaReq{
			pkt:  msg.CliMsg,
			sess: sess,
			what: msg.TopicMsg.MetaReq.What,
			// Impersonate the original session.
			sessOverrides: &sessionOverrides{
				sid:     origSid,
				origReq: msg.TopicMsg.MetaReq,
			},
		}
		log.Printf("cluster: meta request %+v %+v", req, req.pkt)
		if t := globals.hub.topicGet(msg.RcptTo); t != nil {
			t.meta <- req
		} else {
			log.Printf("cluster: meta request for unknown topic %s", msg.RcptTo)
		}

	case msg.TopicMsg.BroadcastReq != nil:
		if msg.SrvMsg == nil {
			panic("cluster: topic proxy broadcast request has no data message")
		}
		msg.SrvMsg.sessOverrides = &sessionOverrides{
			sid:     origSid,
			origReq: msg.TopicMsg.BroadcastReq,
		}
		msg.SrvMsg.sess = sess
		globals.hub.route <- msg.SrvMsg

	case msg.TopicMsg.UAChangeReq != nil:
		if t := globals.hub.topicGet(msg.RcptTo); t != nil {
			t.uaChange <- msg.TopicMsg.UAChangeReq.UserAgent
		} else {
			log.Printf("cluster: UA change request for unknown topic %s", msg.RcptTo)
		}

	case msg.TopicMsg.DefrNotifReq != nil:
		if t := globals.hub.topicGet(msg.RcptTo); t != nil {
			notifReqs := make([]deferredNotification, len(msg.TopicMsg.DefrNotifReq.SendNotificationRequests))
			for i, req := range msg.TopicMsg.DefrNotifReq.SendNotificationRequests {
				notifReqs[i] = deferredNotification{uid: types.ParseUserId(req.AsUser), sid: req.Sid, userAgent: req.UserAgent}
			}

			sess.markRemoteSessionsForeground(notifReqs)
			t.master <- &topicMasterRequest{deferredNotificationsRequest: notifReqs}
		} else {
			log.Printf("cluster: deferred notifications request for unknown topic %s", msg.RcptTo)
		}

	default:
		log.Println("cluster TopicMaster: malformed", msg.RcptTo)
		*rejected = true
	}

	return nil
}

// markRemoteSessionsForeground marks currently tracked remote sessions
// as non-background to ensure proper subscribed user accounting in the master topic.
func (s *Session) markRemoteSessionsForeground(dn []deferredNotification) {
	s.remoteSessionsLock.Lock()
	defer s.remoteSessionsLock.Unlock()
	for i := range dn {
		if rs, ok := s.remoteSessions[dn[i].sid]; ok {
			rs.isBackground = false
			s.remoteSessions[dn[i].sid] = rs
		} else {
			log.Printf("cluster: deferred notifications request for unknown session %s", dn[i].sid)
		}
	}
}

// Proxy receives messages from the master node addressed to a specific local session.
// Called by Session.writeRPC
func (Cluster) Proxy(msg *ClusterResp, unused *bool) error {
	// This cluster member received a response from topic owner to be forwarded to a session
	// Find appropriate session, send the message to it
	if sess := globals.sessionStore.Get(msg.FromSID); sess != nil {
		if !sess.queueOut(msg.SrvMsg) {
			log.Println("cluster.Proxy: timeout")
		}
	} else {
		log.Println("cluster: master response for unknown session", msg.FromSID)
	}

	return nil
}

func (Cluster) TopicProxy(msg *ClusterResp, unused *bool) error {
	// This cluster member received a response from the topic master to be forwarded to the topic.
	// Find appropriate topic, send the message to it.
	if t := globals.hub.topicGet(msg.RcptTo); t != nil {
		t.proxy <- msg
	} else {
		log.Println("cluster: unknown topic name", msg.RcptTo)
	}

	return nil
}

// Route endpoint receives intra-cluster messages (e.g. pres) destined for the nodes hosting topic.
// Called by Hub.route channel consumer.
func (c *Cluster) Route(msg *ClusterReq, rejected *bool) error {
	*rejected = false
	if msg.Signature != c.ring.Signature() {
		sid := ""
		if msg.Sess != nil {
			sid = msg.Sess.Sid
		}
		log.Println("cluster Route: session signature mismatch", sid)
		*rejected = true
		return nil
	}
	if msg.SrvMsg == nil {
		sid := ""
		if msg.Sess != nil {
			sid = msg.Sess.Sid
		}
		// TODO: maybe panic here.
		log.Println("cluster Route: nil server message", sid)
		*rejected = true
		return nil
	}
	globals.hub.route <- msg.SrvMsg
	return nil
}

// User cache & push notifications management. These are calls received by the Master from Proxy.
// The Proxy expects no payload to be returned by the master.

// UserCacheUpdate endpoint receives updates to user's cached values as well as sends push notifications.
func (c *Cluster) UserCacheUpdate(msg *UserCacheReq, rejected *bool) error {
	usersRequestFromCluster(msg)
	return nil
}

// Sends user cache update to user's Master node where the cache actually resides.
// The request is extected to contain users who reside at remote nodes only.
func (c *Cluster) routeUserReq(req *UserCacheReq) error {
	// Index requests by cluster node.
	reqByNode := make(map[string]*UserCacheReq)

	if req.PushRcpt != nil {
		// Request to send push notifications. Create separate packets for each affected cluster node.
		for uid, recipient := range req.PushRcpt.To {
			n := c.nodeForTopic(uid.UserId())
			if n == nil {
				return errors.New("attempt to update user at a non-existent node (1)")
			}
			r := reqByNode[n.name]
			if r == nil {
				r = &UserCacheReq{
					PushRcpt: &push.Receipt{
						Payload: req.PushRcpt.Payload,
						To:      make(map[types.Uid]push.Recipient)},
					Node: c.thisNodeName}
			}
			r.PushRcpt.To[uid] = recipient
			reqByNode[n.name] = r
		}
	} else if len(req.UserIdList) > 0 {
		// Request to add/remove user from cache.
		for _, uid := range req.UserIdList {
			n := c.nodeForTopic(uid.UserId())
			if n == nil {
				return errors.New("attempt to update user at a non-existent node (2)")
			}
			r := reqByNode[n.name]
			if r == nil {
				r = &UserCacheReq{Node: c.thisNodeName, Inc: req.Inc}
			}
			r.UserIdList = append(r.UserIdList, uid)
			reqByNode[n.name] = r
		}
	}

	if len(reqByNode) > 0 {
		for nodeName, r := range reqByNode {
			n := globals.cluster.nodes[nodeName]
			var rejected bool
			err := n.call("Cluster.UserCacheUpdate", r, &rejected)
			if rejected {
				err = errors.New("cluster: master node out of sync")
			}
			if err != nil {
				return err
			}
		}
		return nil
	}

	// Update to cached values.
	n := c.nodeForTopic(req.UserId.UserId())
	if n == nil {
		return errors.New("attempt to update user at a non-existent node (3)")
	}
	req.Node = c.thisNodeName
	var rejected bool
	err := n.call("Cluster.UserCacheUpdate", req, &rejected)
	if rejected {
		err = errors.New("cluster: master node out of sync")
	}
	return err
}

// Given topic name, find appropriate cluster node to route message to
func (c *Cluster) nodeForTopic(topic string) *ClusterNode {
	key := c.ring.Get(topic)
	if key == c.thisNodeName {
		log.Println("cluster: request to route to self")
		// Do not route to self
		return nil
	}

	node := globals.cluster.nodes[key]
	if node == nil {
		log.Println("cluster: no node for topic", topic, key)
	}
	return node
}

// isRemoteTopic checks if the given topic is handled by this node or a remote node.
func (c *Cluster) isRemoteTopic(topic string) bool {
	if c == nil {
		// Cluster not initialized, all topics are local
		return false
	}
	return c.ring.Get(topic) != c.thisNodeName
}

// genLocalTopicName is just like genTopicName(), but the generated name belongs to the current cluster node.
func (c *Cluster) genLocalTopicName() string {
	topic := genTopicName()
	if c == nil {
		// Cluster not initialized, all topics are local
		return topic
	}

	// FIXME: if cluster is large it may become too inefficient.
	for c.ring.Get(topic) != c.thisNodeName {
		topic = genTopicName()
	}
	return topic
}

// Returns remote node name where the topic is hosted.
// If the topic is hosted locally, returns an empty string.
func (c *Cluster) nodeNameForTopicIfRemote(topic string) string {
	if c == nil {
		// Cluster not initialized, all topics are local
		return ""
	}
	key := c.ring.Get(topic)
	if key == c.thisNodeName {
		return ""
	}
	return key
}

// isPartitioned checks if the cluster is partitioned due to network or other failure and if the
// current node is a part of the smaller partition.
func (c *Cluster) isPartitioned() bool {
	if c == nil || c.fo == nil {
		// Cluster not initialized or failover disabled therefore not partitioned.
		return false
	}

	return (len(c.nodes)+1)/2 >= len(c.fo.activeNodes)
}

func (c *Cluster) getClusterReq(cliMsg *ClientComMessage, srvMsg *ServerComMessage,
	topicMsg *ProxyTopicMessage, topic string, sess *Session) *ClusterReq {
	req := &ClusterReq{
		Node:        c.thisNodeName,
		Signature:   c.ring.Signature(),
		Fingerprint: c.fingerprint,
		CliMsg:      cliMsg,
		SrvMsg:      srvMsg,
		TopicMsg:    topicMsg,
		RcptTo:      topic,
	}
	if sess != nil {
		req.Sess = &ClusterSess{
			Uid:        sess.uid,
			AuthLvl:    sess.authLvl,
			RemoteAddr: sess.remoteAddr,
			UserAgent:  sess.userAgent,
			Ver:        sess.ver,
			Lang:       sess.lang,
			DeviceID:   sess.deviceID,
			Platform:   sess.platf,
			Sid:        sess.sid}
	}
	return req
}

// Forward client request message from the Topic Proxy to the Topic Master (cluster node which owns the topic)
func (c *Cluster) routeToTopicMaster(cliMsg *ClientComMessage, srvMsg *ServerComMessage,
	topicMsg *ProxyTopicMessage, topic string, sess *Session) error {
	if c == nil {
		// Cluster may be nil due to shutdown.
		return nil
	}

	// Find the cluster node which owns the topic, then forward to it.
	n := c.nodeForTopic(topic)
	if n == nil {
		return errors.New("node for topic not found")
	}

	req := c.getClusterReq(cliMsg, srvMsg, topicMsg, topic, sess)
	return n.forwardToTopicMaster(req)
}

// Forward server response message to the node that owns topic.
func (c *Cluster) routeToTopicIntraCluster(topic string, msg *ServerComMessage, sess *Session) error {
	if c == nil {
		// Cluster may be nil due to shutdown.
		return nil
	}

	n := c.nodeForTopic(topic)
	if n == nil {
		return errors.New("node for topic not found (intra)")
	}

	req := &ClusterReq{
		Node:        c.thisNodeName,
		Signature:   c.ring.Signature(),
		Fingerprint: c.fingerprint,
		RcptTo:      topic,
		SrvMsg:      msg,
	}

	if sess != nil {
		req.Sess = &ClusterSess{Sid: sess.sid}
	}
	return n.route(req)
}

// Topic proxy terminated. Inform remote Master node that the proxy is gone.
func (c *Cluster) topicProxyGone(topicName string) error {
	if c == nil {
		// Cluster may be nil due to shutdown.
		return nil
	}

	// Find the cluster node which owns the topic, then forward to it.
	n := c.nodeForTopic(topicName)
	if n == nil {
		return errors.New("node for topic not found")
	}

	req := c.getClusterReq(nil, nil, nil, topicName, nil)
	req.Done = true
	return n.forwardToTopicMaster(req)
}

// Returns snowflake worker id
func clusterInit(configString json.RawMessage, self *string) int {
	if globals.cluster != nil {
		log.Fatal("Cluster already initialized.")
	}

	// Registering variables even if it's a standalone server. Otherwise monitoring software will
	// complain about missing vars.

	// 1 if this node is cluster leader, 0 otherwise
	statsRegisterInt("ClusterLeader")
	// Total number of nodes configured
	statsRegisterInt("TotalClusterNodes")
	// Number of nodes currently believed to be up.
	statsRegisterInt("LiveClusterNodes")

	// This is a standalone server, not initializing
	if len(configString) == 0 {
		log.Println("Running as a standalone server.")
		return 1
	}

	var config clusterConfig
	if err := json.Unmarshal(configString, &config); err != nil {
		log.Fatal(err)
	}

	thisName := *self
	if thisName == "" {
		thisName = config.ThisName
	}

	// Name of the current node is not specified - disable clustering
	if thisName == "" {
		log.Println("Running as a standalone server.")
		return 1
	}

	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
	gob.Register(map[string]int{})
	gob.Register(map[string]string{})
	gob.Register(MsgAccessMode{})

	globals.cluster = &Cluster{
		thisNodeName: thisName,
		fingerprint:  time.Now().Unix(),
		nodes:        make(map[string]*ClusterNode)}

	var nodeNames []string
	for _, host := range config.Nodes {
		nodeNames = append(nodeNames, host.Name)

		if host.Name == thisName {
			globals.cluster.listenOn = host.Addr
			// Don't create a cluster member for this local instance
			continue
		}

		globals.cluster.nodes[host.Name] = &ClusterNode{
			address: host.Addr,
			name:    host.Name,
			done:    make(chan bool, 1)}
	}

	if len(globals.cluster.nodes) == 0 {
		// Cluster needs at least two nodes.
		log.Fatal("Invalid cluster size: 1")
	}

	if !globals.cluster.failoverInit(config.Failover) {
		globals.cluster.rehash(nil)
	}

	sort.Strings(nodeNames)
	workerId := sort.SearchStrings(nodeNames, thisName) + 1

	statsSet("TotalClusterNodes", int64(len(globals.cluster.nodes)+1))

	return workerId
}

// This is a session handler at a master node: forward messages from the master to the session origin.
func (sess *Session) rpcWriteLoop() {
	// There is no readLoop for RPC, delete the session here
	defer func() {
		sess.closeRPC()
		globals.sessionStore.Delete(sess)
		sess.unsubAll()
	}()

	// Timer which checks for orphaned nodes.
	heartBeat := time.NewTimer(clusterSessionCleanup)

	for {
		select {
		case msg, ok := <-sess.send:
			if !ok || sess.clnode.endpoint == nil {
				// channel closed
				return
			}
			// The error is returned if the remote node is down. Which means the remote
			// session is also disconnected.
			if err := sess.clnode.respond(&ClusterResp{SrvMsg: msg.(*ServerComMessage), FromSID: sess.sid}); err != nil {
				log.Println("cluster: sess.writeRPC:", err)
				return
			}
		case msg := <-sess.stop:
			// Shutdown is requested, don't care if the message is delivered
			if msg != nil {
				sess.clnode.respond(&ClusterResp{SrvMsg: msg.(*ServerComMessage), FromSID: sess.sid})
			}
			return

		case topic := <-sess.detach:
			sess.delSub(topic)

		case <-heartBeat.C:
			// All proxied subsriptions are gone, this session is no longer needed.
			if sess.countSub() == 0 {
				return
			}
		}
	}
}

// Proxied session is being closed at the Master node
func (sess *Session) closeRPC() {
	if sess.proto == CLUSTER {
		log.Println("cluster: session proxy closed", sess.sid)
	}
}

// Start accepting connections.
func (c *Cluster) start() {
	addr, err := net.ResolveTCPAddr("tcp", c.listenOn)
	if err != nil {
		log.Fatal(err)
	}

	c.inbound, err = net.ListenTCP("tcp", addr)

	if err != nil {
		log.Fatal(err)
	}

	for _, n := range c.nodes {
		go n.reconnect()
	}

	if c.fo != nil {
		go c.run()
	}

	err = rpc.Register(c)
	if err != nil {
		log.Fatal(err)
	}

	go rpc.Accept(c.inbound)

	log.Printf("Cluster of %d nodes initialized, node '%s' listening on [%s]", len(globals.cluster.nodes)+1,
		globals.cluster.thisNodeName, c.listenOn)
}

func (c *Cluster) shutdown() {
	if globals.cluster == nil {
		return
	}
	globals.cluster = nil

	c.inbound.Close()

	if c.fo != nil {
		c.fo.done <- true
	}

	for _, n := range c.nodes {
		n.done <- true
	}

	log.Println("Cluster shut down")
}

// Recalculate the ring hash using provided list of nodes or only nodes in a non-failed state.
// Returns the list of nodes used for ring hash.
func (c *Cluster) rehash(nodes []string) []string {
	ring := rh.New(clusterHashReplicas, nil)

	var ringKeys []string

	if nodes == nil {
		for _, node := range c.nodes {
			ringKeys = append(ringKeys, node.name)
		}
		ringKeys = append(ringKeys, c.thisNodeName)
	} else {
		ringKeys = append(ringKeys, nodes...)
	}
	ring.Add(ringKeys...)

	c.ring = ring

	return ringKeys
}

// invalidateProxySubs iterates over sessions proxied on this node and for each session
// sends "{pres term}" to all displayed topics.
// Called immediately after Cluster.rehash().
// TODO: consider resubscribing to the new master topics instead of forcing sessions to resubscribe.
func (c *Cluster) invalidateProxySubs() {
	sessions := make(map[*Session][]string)
	globals.hub.topics.Range(func(_, v interface{}) bool {
		topic := v.(*Topic)
		if !topic.isProxy || topic.masterNode == c.ring.Get(topic.name) {
			// Topic either isn't a proxy or hasn't moved. Continue.
			return true
		}
		for s, _ := range topic.sessions {
			sessions[s] = append(sessions[s], topic.name)
		}
		return true
	})
	for s, topicsToTerminate := range sessions {
		s.presTermDirect(topicsToTerminate)
	}
}

// garbageCollectProxySessions terminates all orphaned proxy sessions
// (whose origin nodes are gone).
func (c *Cluster) garbageCollectProxySessions(activeNodes []string) {
	sessions := make(map[*Session]*Topic)
	activeNodeMap := make(map[string]bool)
	for _, n := range activeNodes {
		activeNodeMap[n] = true
	}
	globals.hub.topics.Range(func(_, v interface{}) bool {
		topic := v.(*Topic)
		if topic.isProxy {
			// Topic is a proxy. Continue.
			return true
		}
		for s := range topic.sessions {
			if s.isProxy() {
				if _, originActive := activeNodeMap[s.clnode.name]; !originActive {
					// Session's origin is no longer active.
					sessions[s] = topic
				}
			}
		}
		return true
	})
	// Unsubscribe orphaned sessions from their master topics.
	// Stop orphaned sessions.
	for s, t := range sessions {
		t.unreg <- &sessionLeave{
			terminateProxyConnection: true,
			sess:                     s,
		}
		s.stop <- nil
	}
}

// cleanUpRemoteSubs adjusts online user counts in the master (proxied) topic
// for a leaving proxy session.
func (sess *Session) cleanUpRemoteSessions(topicName string) {
	sess.remoteSessionsLock.Lock()
	uidCounts := make(map[types.Uid]int)
	for _, rs := range sess.remoteSessions {
		if !rs.isBackground {
			// Only non-background sessions are counted as online.
			uidCounts[rs.uid]++
		}
	}
	sess.remoteSessionsLock.Unlock()
	if t := globals.hub.topicGet(topicName); t != nil {
		t.master <- &topicMasterRequest{proxySessionCleanUp: uidCounts}
	} else {
		log.Printf("cluster: remote subscription clean up unknown topic %s", topicName)
	}
}

// topicProxyWriteLoop implements proxy session event loop.
func (sess *Session) topicProxyWriteLoop(forTopic string) {
	defer func() {
		sess.closeRPC()
		globals.sessionStore.Delete(sess)
		sess.unsubAll()
		sess.cleanUpRemoteSessions(forTopic)
	}()

	for {
		select {
		case msg, ok := <-sess.send:
			if !ok || sess.clnode.endpoint == nil {
				// channel closed
				return
			}
			srvMsg := msg.(*ServerComMessage)

			response := &ClusterResp{SrvMsg: srvMsg}
			copyParamsFromSession := false
			if srvMsg.sessOverrides != nil {
				switch req := srvMsg.sessOverrides.origReq.(type) {
				case nil:
					panic("cluster: origReq is nil in session overrides")
				case *ProxyJoin:
					response.OrigRequestType = ProxyRequestJoin
					response.IsBackground = req.IsBackground
					response.Uid = types.ParseUserId(srvMsg.AsUser)
					sess.addRemoteSession(srvMsg.OrigSid, &remoteSession{
						uid:          response.Uid,
						isBackground: response.IsBackground})
				case *ProxyLeave:
					response.OrigRequestType = ProxyRequestLeave
					sess.delRemoteSession(srvMsg.OrigSid)
					if req.TerminateProxyConnection {
						log.Printf("session [%s]: terminating upon client request", srvMsg.OrigSid)
						sess.detach <- forTopic
					}
				case *ProxyBroadcast:
					response.OrigRequestType = ProxyRequestBroadcast
				case *ProxyMeta:
					response.OrigRequestType = ProxyRequestMeta
				}
				copyParamsFromSession = srvMsg.OrigSid != ""
			} else {
				// Copy skipSid.
				if srvMsg.Ctrl == nil && srvMsg.Data == nil && srvMsg.Pres == nil && srvMsg.Info == nil {
					// Only broadcast messages (data, pres, info) may come not as a response to a client request.
					log.Panicf("cluster: only broadcast messages may not contain session overrides: %+v", srvMsg)
				}
				response.Uid = srvMsg.uid
			}
			if copyParamsFromSession {
				// Reply to a specific session.
				response.FromSID = srvMsg.OrigSid
				response.RcptTo = srvMsg.RcptTo
			} else {
				response.RcptTo = forTopic
				response.FromSID = "*"
			}

			if err := sess.clnode.respondToTopicProxy(response); err != nil {
				log.Printf("cluster tp [%s]: sess.topicProxyWrite: %s", sess.sid, err.Error())
				return
			}
		case msg := <-sess.stop:
			if msg != nil {
				log.Println("sess stop", msg.(*ServerComMessage), " to ", sess.clnode.endpoint, " from ", sess.sid)
			}
			return

		case <-sess.detach:
			return
		}
	}
}
