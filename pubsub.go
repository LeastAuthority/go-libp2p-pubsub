package pubsub

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/libp2p/go-libp2p-pubsub/pb"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/discovery"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"

	logging "github.com/ipfs/go-log"
	timecache "github.com/whyrusleeping/timecache"
)

// DefaultMaximumMessageSize is 1mb.
const DefaultMaxMessageSize = 1 << 20

var (
	TimeCacheDuration = 120 * time.Second
)

var log = logging.Logger("pubsub")

// PubSub is the implementation of the pubsub system.
type PubSub struct {
	// atomic counter for seqnos
	// NOTE: Must be declared at the top of the struct as we perform atomic
	// operations on this field.
	//
	// See: https://golang.org/pkg/sync/atomic/#pkg-note-BUG
	counter uint64

	host host.Host

	rt PubSubRouter

	val *validation

	disc *discover

	tracer *pubsubTracer

	// maxMessageSize is the maximum message size; it applies globally to all
	// topics.
	maxMessageSize int

	// size of the outbound message channel that we maintain for each peer
	peerOutboundQueueSize int

	// incoming messages from other peers
	incoming chan *RPC

	// messages we are publishing out to our peers
	publish chan *Message

	// addSub is a control channel for us to add and remove subscriptions
	addSub chan *addSubReq

	// get list of topics we are subscribed to
	getTopics chan *topicReq

	// get chan of peers we are connected to
	getPeers chan *listPeerReq

	// send subscription here to cancel it
	cancelCh chan *Subscription

	// addSub is a channel for us to add a topic
	addTopic chan *addTopicReq

	// removeTopic is a topic cancellation channel
	rmTopic chan *rmTopicReq

	// a notification channel for new peer connections
	newPeers chan peer.ID

	// a notification channel for new outoging peer streams
	newPeerStream chan network.Stream

	// a notification channel for errors opening new peer streams
	newPeerError chan peer.ID

	// a notification channel for when our peers die
	peerDead chan peer.ID

	// The set of topics we are subscribed to
	mySubs map[string]map[*Subscription]struct{}

	// The set of topics we are interested in
	myTopics map[string]*Topic

	// topics tracks which topics each of our peers are subscribed to
	topics map[string]map[peer.ID]struct{}

	// sendMsg handles messages that have been validated
	sendMsg chan *Message

	// addVal handles validator registration requests
	addVal chan *addValReq

	// rmVal handles validator unregistration requests
	rmVal chan *rmValReq

	// eval thunk in event loop
	eval chan func()

	// peer blacklist
	blacklist     Blacklist
	blacklistPeer chan peer.ID

	peers map[peer.ID]chan *RPC

	seenMessagesMx sync.Mutex
	seenMessages   *timecache.TimeCache

	// function used to compute the ID for a message
	msgID MsgIdFunction

	// key for signing messages; nil when signing is disabled (default for now)
	signKey crypto.PrivKey
	// source ID for signed messages; corresponds to signKey
	signID peer.ID
	// strict mode rejects all unsigned messages prior to validation
	signStrict bool

	ctx context.Context
}

// PubSubRouter is the message router component of PubSub.
type PubSubRouter interface {
	// Protocols returns the list of protocols supported by the router.
	Protocols() []protocol.ID
	// Attach is invoked by the PubSub constructor to attach the router to a
	// freshly initialized PubSub instance.
	Attach(*PubSub)
	// AddPeer notifies the router that a new peer has been connected.
	AddPeer(peer.ID, protocol.ID)
	// RemovePeer notifies the router that a peer has been disconnected.
	RemovePeer(peer.ID)
	// EnoughPeers returns whether the router needs more peers before it's ready to publish new records.
	// Suggested (if greater than 0) is a suggested number of peers that the router should need.
	EnoughPeers(topic string, suggested int) bool
	// AcceptFrom is invoked on any incoming message before pushing it to the validation pipeline
	// or processing control information.
	// Allows routers with internal scoring to vet peers before commiting any processing resources
	// to the message and implement an affective graylist.
	AcceptFrom(peer.ID) bool
	// HandleRPC is invoked to process control messages in the RPC envelope.
	// It is invoked after subscriptions and payload messages have been processed.
	HandleRPC(*RPC)
	// Publish is invoked to forward a new message that has been validated.
	Publish(*Message)
	// Join notifies the router that we want to receive and forward messages in a topic.
	// It is invoked after the subscription announcement.
	Join(topic string)
	// Leave notifies the router that we are no longer interested in a topic.
	// It is invoked after the unsubscription announcement.
	Leave(topic string)
}

type Message struct {
	*pb.Message
	ReceivedFrom  peer.ID
	ValidatorData interface{}
}

func (m *Message) GetFrom() peer.ID {
	return peer.ID(m.Message.GetFrom())
}

type RPC struct {
	pb.RPC

	// unexported on purpose, not sending this over the wire
	from peer.ID
}

type Option func(*PubSub) error

// NewPubSub returns a new PubSub management object.
func NewPubSub(ctx context.Context, h host.Host, rt PubSubRouter, opts ...Option) (*PubSub, error) {
	ps := &PubSub{
		host:                  h,
		ctx:                   ctx,
		rt:                    rt,
		val:                   newValidation(),
		disc:                  &discover{},
		maxMessageSize:        DefaultMaxMessageSize,
		peerOutboundQueueSize: 32,
		signID:                h.ID(),
		signKey:               h.Peerstore().PrivKey(h.ID()),
		signStrict:            true,
		incoming:              make(chan *RPC, 32),
		publish:               make(chan *Message),
		newPeers:              make(chan peer.ID),
		newPeerStream:         make(chan network.Stream),
		newPeerError:          make(chan peer.ID),
		peerDead:              make(chan peer.ID),
		cancelCh:              make(chan *Subscription),
		getPeers:              make(chan *listPeerReq),
		addSub:                make(chan *addSubReq),
		addTopic:              make(chan *addTopicReq),
		rmTopic:               make(chan *rmTopicReq),
		getTopics:             make(chan *topicReq),
		sendMsg:               make(chan *Message, 32),
		addVal:                make(chan *addValReq),
		rmVal:                 make(chan *rmValReq),
		eval:                  make(chan func()),
		myTopics:              make(map[string]*Topic),
		mySubs:                make(map[string]map[*Subscription]struct{}),
		topics:                make(map[string]map[peer.ID]struct{}),
		peers:                 make(map[peer.ID]chan *RPC),
		blacklist:             NewMapBlacklist(),
		blacklistPeer:         make(chan peer.ID),
		seenMessages:          timecache.NewTimeCache(TimeCacheDuration),
		msgID:                 DefaultMsgIdFn,
		counter:               uint64(time.Now().UnixNano()),
	}

	for _, opt := range opts {
		err := opt(ps)
		if err != nil {
			return nil, err
		}
	}

	if ps.signStrict && ps.signKey == nil {
		return nil, fmt.Errorf("strict signature verification enabled but message signing is disabled")
	}

	if err := ps.disc.Start(ps); err != nil {
		return nil, err
	}

	rt.Attach(ps)

	for _, id := range rt.Protocols() {
		h.SetStreamHandler(id, ps.handleNewStream)
	}
	h.Network().Notify((*PubSubNotif)(ps))

	ps.val.Start(ps)

	go ps.processLoop(ctx)

	return ps, nil
}

// MsgIdFunction returns a unique ID for the passed Message, and PubSub can be customized to use any
// implementation of this function by configuring it with the Option from WithMessageIdFn.
type MsgIdFunction func(pmsg *pb.Message) string

// WithMessageIdFn is an option to customize the way a message ID is computed for a pubsub message.
// The default ID function is DefaultMsgIdFn (concatenate source and seq nr.),
// but it can be customized to e.g. the hash of the message.
func WithMessageIdFn(fn MsgIdFunction) Option {
	return func(p *PubSub) error {
		p.msgID = fn
		// the tracer Option may already be set. Update its message ID function to make options order-independent.
		if p.tracer != nil {
			p.tracer.msgID = fn
		}
		return nil
	}
}

// WithPeerOutboundQueueSize is an option to set the buffer size for outbound messages to a peer
// We start dropping messages to a peer if the outbound queue if full
func WithPeerOutboundQueueSize(size int) Option {
	return func(p *PubSub) error {
		if size <= 0 {
			return errors.New("outbound queue size must always be positive")
		}
		p.peerOutboundQueueSize = size
		return nil
	}
}

// WithMessageSigning enables or disables message signing (enabled by default).
func WithMessageSigning(enabled bool) Option {
	return func(p *PubSub) error {
		if enabled {
			p.signKey = p.host.Peerstore().PrivKey(p.signID)
			if p.signKey == nil {
				return fmt.Errorf("can't sign for peer %s: no private key", p.signID)
			}
		} else {
			p.signKey = nil
			p.signStrict = false
		}
		return nil
	}
}

// WithMessageAuthor sets the author for outbound messages to the given peer ID
// (defaults to the host's ID). If message signing is enabled, the private key
// must be available in the host's peerstore.
func WithMessageAuthor(author peer.ID) Option {
	return func(p *PubSub) error {
		if author == "" {
			author = p.host.ID()
		}
		if p.signKey != nil {
			newSignKey := p.host.Peerstore().PrivKey(author)
			if newSignKey == nil {
				return fmt.Errorf("can't sign for peer %s: no private key", p.signID)
			}
			p.signKey = newSignKey
		}
		p.signID = author
		return nil
	}
}

// WithStrictSignatureVerification is an option to enable or disable strict message signing.
// When enabled (which is the default), unsigned messages will be discarded.
func WithStrictSignatureVerification(required bool) Option {
	return func(p *PubSub) error {
		p.signStrict = required
		return nil
	}
}

// WithBlacklist provides an implementation of the blacklist; the default is a
// MapBlacklist
func WithBlacklist(b Blacklist) Option {
	return func(p *PubSub) error {
		p.blacklist = b
		return nil
	}
}

// WithDiscovery provides a discovery mechanism used to bootstrap and provide peers into PubSub
func WithDiscovery(d discovery.Discovery, opts ...DiscoverOpt) Option {
	return func(p *PubSub) error {
		discoverOpts := defaultDiscoverOptions()
		for _, opt := range opts {
			err := opt(discoverOpts)
			if err != nil {
				return err
			}
		}

		p.disc.discovery = &pubSubDiscovery{Discovery: d, opts: discoverOpts.opts}
		p.disc.options = discoverOpts
		return nil
	}
}

// WithEventTracer provides a tracer for the pubsub system
func WithEventTracer(tracer EventTracer) Option {
	return func(p *PubSub) error {
		if p.tracer != nil {
			p.tracer.tracer = tracer
		} else {
			p.tracer = &pubsubTracer{tracer: tracer, pid: p.host.ID(), msgID: p.msgID}
		}
		return nil
	}
}

// WithMaxMessageSize sets the global maximum message size for pubsub wire
// messages. The default value is 1MiB (DefaultMaxMessageSize).
//
// Observe the following warnings when setting this option.
//
// WARNING #1: Make sure to change the default protocol prefixes for floodsub
// (FloodSubID) and gossipsub (GossipSubID). This avoids accidentally joining
// the public default network, which uses the default max message size, and
// therefore will cause messages to be dropped.
//
// WARNING #2: Reducing the default max message limit is fine, if you are
// certain that your application messages will not exceed the new limit.
// However, be wary of increasing the limit, as pubsub networks are naturally
// write-amplifying, i.e. for every message we receive, we send D copies of the
// message to our peers. If those messages are large, the bandwidth requirements
// will grow linearly. Note that propagation is sent on the uplink, which
// traditionally is more constrained than the downlink. Instead, consider
// out-of-band retrieval for large messages, by sending a CID (Content-ID) or
// another type of locator, such that messages can be fetched on-demand, rather
// than being pushed proactively. Under this design, you'd use the pubsub layer
// as a signalling system, rather than a data delivery system.
func WithMaxMessageSize(maxMessageSize int) Option {
	return func(ps *PubSub) error {
		ps.maxMessageSize = maxMessageSize
		return nil
	}
}

// processLoop handles all inputs arriving on the channels
func (p *PubSub) processLoop(ctx context.Context) {
	defer func() {
		// Clean up go routines.
		for _, ch := range p.peers {
			close(ch)
		}
		p.peers = nil
		p.topics = nil
	}()

	for {
		select {
		case pid := <-p.newPeers:
			if _, ok := p.peers[pid]; ok {
				log.Warning("already have connection to peer: ", pid)
				continue
			}

			if p.blacklist.Contains(pid) {
				log.Warning("ignoring connection from blacklisted peer: ", pid)
				continue
			}

			messages := make(chan *RPC, p.peerOutboundQueueSize)
			messages <- p.getHelloPacket()
			go p.handleNewPeer(ctx, pid, messages)
			p.peers[pid] = messages

		case s := <-p.newPeerStream:
			pid := s.Conn().RemotePeer()

			ch, ok := p.peers[pid]
			if !ok {
				log.Warning("new stream for unknown peer: ", pid)
				s.Reset()
				continue
			}

			if p.blacklist.Contains(pid) {
				log.Warning("closing stream for blacklisted peer: ", pid)
				close(ch)
				s.Reset()
				continue
			}

			p.rt.AddPeer(pid, s.Protocol())

		case pid := <-p.newPeerError:
			delete(p.peers, pid)

		case pid := <-p.peerDead:
			ch, ok := p.peers[pid]
			if !ok {
				continue
			}

			close(ch)

			if p.host.Network().Connectedness(pid) == network.Connected {
				// still connected, must be a duplicate connection being closed.
				// we respawn the writer as we need to ensure there is a stream active
				log.Warning("peer declared dead but still connected; respawning writer: ", pid)
				messages := make(chan *RPC, p.peerOutboundQueueSize)
				messages <- p.getHelloPacket()
				go p.handleNewPeer(ctx, pid, messages)
				p.peers[pid] = messages
				continue
			}

			delete(p.peers, pid)
			for t, tmap := range p.topics {
				if _, ok := tmap[pid]; ok {
					delete(tmap, pid)
					p.notifyLeave(t, pid)
				}
			}

			p.rt.RemovePeer(pid)

		case treq := <-p.getTopics:
			var out []string
			for t := range p.mySubs {
				out = append(out, t)
			}
			treq.resp <- out
		case topic := <-p.addTopic:
			p.handleAddTopic(topic)
		case topic := <-p.rmTopic:
			p.handleRemoveTopic(topic)
		case sub := <-p.cancelCh:
			p.handleRemoveSubscription(sub)
		case sub := <-p.addSub:
			p.handleAddSubscription(sub)
		case preq := <-p.getPeers:
			tmap, ok := p.topics[preq.topic]
			if preq.topic != "" && !ok {
				preq.resp <- nil
				continue
			}
			var peers []peer.ID
			for p := range p.peers {
				if preq.topic != "" {
					_, ok := tmap[p]
					if !ok {
						continue
					}
				}
				peers = append(peers, p)
			}
			preq.resp <- peers
		case rpc := <-p.incoming:
			p.handleIncomingRPC(rpc)

		case msg := <-p.publish:
			p.tracer.PublishMessage(msg)
			p.pushMsg(msg)

		case msg := <-p.sendMsg:
			p.publishMessage(msg)

		case req := <-p.addVal:
			p.val.AddValidator(req)

		case req := <-p.rmVal:
			p.val.RemoveValidator(req)

		case thunk := <-p.eval:
			thunk()

		case pid := <-p.blacklistPeer:
			log.Infof("Blacklisting peer %s", pid)
			p.blacklist.Add(pid)

			ch, ok := p.peers[pid]
			if ok {
				close(ch)
				delete(p.peers, pid)
				for t, tmap := range p.topics {
					if _, ok := tmap[pid]; ok {
						delete(tmap, pid)
						p.notifyLeave(t, pid)
					}
				}
				p.rt.RemovePeer(pid)
			}

		case <-ctx.Done():
			log.Info("pubsub processloop shutting down")
			return
		}
	}
}

// handleAddTopic adds a tracker for a particular topic.
// Only called from processLoop.
func (p *PubSub) handleAddTopic(req *addTopicReq) {
	topic := req.topic
	topicID := topic.topic

	t, ok := p.myTopics[topicID]
	if ok {
		req.resp <- t
		return
	}

	p.myTopics[topicID] = topic
	req.resp <- topic
}

// handleRemoveTopic removes Topic tracker from bookkeeping.
// Only called from processLoop.
func (p *PubSub) handleRemoveTopic(req *rmTopicReq) {
	topic := p.myTopics[req.topic.topic]

	if topic == nil {
		req.resp <- nil
		return
	}

	if len(topic.evtHandlers) == 0 && len(p.mySubs[req.topic.topic]) == 0 {
		delete(p.myTopics, topic.topic)
		req.resp <- nil
		return
	}

	req.resp <- fmt.Errorf("cannot close topic: outstanding event handlers or subscriptions")
}

// handleRemoveSubscription removes Subscription sub from bookeeping.
// If this was the last Subscription for a given topic, it will also announce
// that this node is not subscribing to this topic anymore.
// Only called from processLoop.
func (p *PubSub) handleRemoveSubscription(sub *Subscription) {
	subs := p.mySubs[sub.topic]

	if subs == nil {
		return
	}

	sub.err = fmt.Errorf("subscription cancelled by calling sub.Cancel()")
	sub.close()
	delete(subs, sub)

	if len(subs) == 0 {
		delete(p.mySubs, sub.topic)
		p.disc.StopAdvertise(sub.topic)
		p.announce(sub.topic, false)
		p.rt.Leave(sub.topic)
	}
}

// handleAddSubscription adds a Subscription for a particular topic. If it is
// the first Subscription for the topic, it will announce that this node
// subscribes to the topic.
// Only called from processLoop.
func (p *PubSub) handleAddSubscription(req *addSubReq) {
	sub := req.sub
	subs := p.mySubs[sub.topic]

	// announce we want this topic
	if len(subs) == 0 {
		p.disc.Advertise(sub.topic)
		p.announce(sub.topic, true)
		p.rt.Join(sub.topic)
	}

	// make new if not there
	if subs == nil {
		p.mySubs[sub.topic] = make(map[*Subscription]struct{})
	}

	sub.cancelCh = p.cancelCh

	p.mySubs[sub.topic][sub] = struct{}{}

	req.resp <- sub
}

// announce announces whether or not this node is interested in a given topic
// Only called from processLoop.
func (p *PubSub) announce(topic string, sub bool) {
	subopt := &pb.RPC_SubOpts{
		Topicid:   &topic,
		Subscribe: &sub,
	}

	out := rpcWithSubs(subopt)
	for pid, peer := range p.peers {
		select {
		case peer <- out:
			p.tracer.SendRPC(out, pid)
		default:
			log.Infof("Can't send announce message to peer %s: queue full; scheduling retry", pid)
			p.tracer.DropRPC(out, pid)
			go p.announceRetry(pid, topic, sub)
		}
	}
}

func (p *PubSub) announceRetry(pid peer.ID, topic string, sub bool) {
	time.Sleep(time.Duration(1+rand.Intn(1000)) * time.Millisecond)

	retry := func() {
		_, ok := p.mySubs[topic]
		if (ok && sub) || (!ok && !sub) {
			p.doAnnounceRetry(pid, topic, sub)
		}
	}

	select {
	case p.eval <- retry:
	case <-p.ctx.Done():
	}
}

func (p *PubSub) doAnnounceRetry(pid peer.ID, topic string, sub bool) {
	peer, ok := p.peers[pid]
	if !ok {
		return
	}

	subopt := &pb.RPC_SubOpts{
		Topicid:   &topic,
		Subscribe: &sub,
	}

	out := rpcWithSubs(subopt)
	select {
	case peer <- out:
		p.tracer.SendRPC(out, pid)
	default:
		log.Infof("Can't send announce message to peer %s: queue full; scheduling retry", pid)
		p.tracer.DropRPC(out, pid)
		go p.announceRetry(pid, topic, sub)
	}
}

// notifySubs sends a given message to all corresponding subscribers.
// Only called from processLoop.
func (p *PubSub) notifySubs(msg *Message) {
	for _, topic := range msg.GetTopicIDs() {
		subs := p.mySubs[topic]
		for f := range subs {
			select {
			case f.ch <- msg:
			default:
				log.Infof("Can't deliver message to subscription for topic %s; subscriber too slow", topic)
			}
		}
	}
}

// seenMessage returns whether we already saw this message before
func (p *PubSub) seenMessage(id string) bool {
	p.seenMessagesMx.Lock()
	defer p.seenMessagesMx.Unlock()
	return p.seenMessages.Has(id)
}

// markSeen marks a message as seen such that seenMessage returns `true' for the given id
// returns true if the message was freshly marked
func (p *PubSub) markSeen(id string) bool {
	p.seenMessagesMx.Lock()
	defer p.seenMessagesMx.Unlock()
	if p.seenMessages.Has(id) {
		return false
	}

	p.seenMessages.Add(id)
	return true
}

// subscribedToMessage returns whether we are subscribed to one of the topics
// of a given message
func (p *PubSub) subscribedToMsg(msg *pb.Message) bool {
	if len(p.mySubs) == 0 {
		return false
	}

	for _, t := range msg.GetTopicIDs() {
		if _, ok := p.mySubs[t]; ok {
			return true
		}
	}
	return false
}

func (p *PubSub) notifyLeave(topic string, pid peer.ID) {
	if t, ok := p.myTopics[topic]; ok {
		t.sendNotification(PeerEvent{PeerLeave, pid})
	}
}

func (p *PubSub) handleIncomingRPC(rpc *RPC) {
	p.tracer.RecvRPC(rpc)

	for _, subopt := range rpc.GetSubscriptions() {
		t := subopt.GetTopicid()
		if subopt.GetSubscribe() {
			tmap, ok := p.topics[t]
			if !ok {
				tmap = make(map[peer.ID]struct{})
				p.topics[t] = tmap
			}

			if _, ok = tmap[rpc.from]; !ok {
				tmap[rpc.from] = struct{}{}
				if topic, ok := p.myTopics[t]; ok {
					peer := rpc.from
					topic.sendNotification(PeerEvent{PeerJoin, peer})
				}
			}
		} else {
			tmap, ok := p.topics[t]
			if !ok {
				continue
			}

			if _, ok := tmap[rpc.from]; ok {
				delete(tmap, rpc.from)
				p.notifyLeave(t, rpc.from)
			}
		}
	}

	// ask the router to vet the peer before commiting any processing resources
	if !p.rt.AcceptFrom(rpc.from) {
		log.Warningf("received message from router graylisted peer %s. Dropping RPC", rpc.from)
		return
	}

	for _, pmsg := range rpc.GetPublish() {
		if !p.subscribedToMsg(pmsg) {
			log.Warning("received message we didn't subscribe to. Dropping.")
			continue
		}

		msg := &Message{pmsg, rpc.from, nil}
		p.pushMsg(msg)
	}

	p.rt.HandleRPC(rpc)
}

// DefaultMsgIdFn returns a unique ID of the passed Message
func DefaultMsgIdFn(pmsg *pb.Message) string {
	return string(pmsg.GetFrom()) + string(pmsg.GetSeqno())
}

// pushMsg pushes a message performing validation as necessary
func (p *PubSub) pushMsg(msg *Message) {
	src := msg.ReceivedFrom
	// reject messages from blacklisted peers
	if p.blacklist.Contains(src) {
		log.Warningf("dropping message from blacklisted peer %s", src)
		p.tracer.RejectMessage(msg, rejectBlacklstedPeer)
		return
	}

	// even if they are forwarded by good peers
	if p.blacklist.Contains(msg.GetFrom()) {
		log.Warningf("dropping message from blacklisted source %s", src)
		p.tracer.RejectMessage(msg, rejectBlacklistedSource)
		return
	}

	// reject unsigned messages when strict before we even process the id
	if p.signStrict && msg.Signature == nil {
		log.Debugf("dropping unsigned message from %s", src)
		p.tracer.RejectMessage(msg, rejectMissingSignature)
		return
	}

	// reject messages claiming to be from ourselves but not locally published
	self := p.host.ID()
	if peer.ID(msg.GetFrom()) == self && src != self {
		log.Debugf("dropping message claiming to be from self but forwarded from %s", src)
		p.tracer.RejectMessage(msg, rejectSelfOrigin)
		return
	}

	// have we already seen and validated this message?
	id := p.msgID(msg.Message)
	if p.seenMessage(id) {
		p.tracer.DuplicateMessage(msg)
		return
	}

	if !p.val.Push(src, msg) {
		return
	}

	if p.markSeen(id) {
		p.publishMessage(msg)
	}
}

func (p *PubSub) publishMessage(msg *Message) {
	p.tracer.DeliverMessage(msg)
	p.notifySubs(msg)
	p.rt.Publish(msg)
}

type addTopicReq struct {
	topic *Topic
	resp  chan *Topic
}

type rmTopicReq struct {
	topic *Topic
	resp  chan error
}

type TopicOptions struct{}

type TopicOpt func(t *Topic) error

// Join joins the topic and returns a Topic handle. Only one Topic handle should exist per topic, and Join will error if
// the Topic handle already exists.
func (p *PubSub) Join(topic string, opts ...TopicOpt) (*Topic, error) {
	t, ok, err := p.tryJoin(topic, opts...)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, fmt.Errorf("topic already exists")
	}

	return t, nil
}

// tryJoin is an internal function that tries to join a topic
// Returns the topic if it can be created or found
// Returns true if the topic was newly created, false otherwise
// Can be removed once pubsub.Publish() and pubsub.Subscribe() are removed
func (p *PubSub) tryJoin(topic string, opts ...TopicOpt) (*Topic, bool, error) {
	t := &Topic{
		p:           p,
		topic:       topic,
		evtHandlers: make(map[*TopicEventHandler]struct{}),
	}

	for _, opt := range opts {
		err := opt(t)
		if err != nil {
			return nil, false, err
		}
	}

	resp := make(chan *Topic, 1)
	select {
	case t.p.addTopic <- &addTopicReq{
		topic: t,
		resp:  resp,
	}:
	case <-t.p.ctx.Done():
		return nil, false, t.p.ctx.Err()
	}
	returnedTopic := <-resp

	if returnedTopic != t {
		return returnedTopic, false, nil
	}

	return t, true, nil
}

type addSubReq struct {
	sub  *Subscription
	resp chan *Subscription
}

type SubOpt func(sub *Subscription) error

// Subscribe returns a new Subscription for the given topic.
// Note that subscription is not an instanteneous operation. It may take some time
// before the subscription is processed by the pubsub main loop and propagated to our peers.
//
// Deprecated: use pubsub.Join() and topic.Subscribe() instead
func (p *PubSub) Subscribe(topic string, opts ...SubOpt) (*Subscription, error) {
	td := pb.TopicDescriptor{Name: &topic}

	return p.SubscribeByTopicDescriptor(&td, opts...)
}

// SubscribeByTopicDescriptor lets you subscribe a topic using a pb.TopicDescriptor.
//
// Deprecated: use pubsub.Join() and topic.Subscribe() instead
func (p *PubSub) SubscribeByTopicDescriptor(td *pb.TopicDescriptor, opts ...SubOpt) (*Subscription, error) {
	if td.GetAuth().GetMode() != pb.TopicDescriptor_AuthOpts_NONE {
		return nil, fmt.Errorf("auth mode not yet supported")
	}

	if td.GetEnc().GetMode() != pb.TopicDescriptor_EncOpts_NONE {
		return nil, fmt.Errorf("encryption mode not yet supported")
	}

	// ignore whether the topic was newly created or not, since either way we have a valid topic to work with
	topic, _, err := p.tryJoin(td.GetName())
	if err != nil {
		return nil, err
	}

	return topic.Subscribe(opts...)
}

type topicReq struct {
	resp chan []string
}

// GetTopics returns the topics this node is subscribed to.
func (p *PubSub) GetTopics() []string {
	out := make(chan []string, 1)
	select {
	case p.getTopics <- &topicReq{resp: out}:
	case <-p.ctx.Done():
		return nil
	}
	return <-out
}

// Publish publishes data to the given topic.
//
// Deprecated: use pubsub.Join() and topic.Publish() instead
func (p *PubSub) Publish(topic string, data []byte, opts ...PubOpt) error {
	// ignore whether the topic was newly created or not, since either way we have a valid topic to work with
	t, _, err := p.tryJoin(topic)
	if err != nil {
		return err
	}

	return t.Publish(context.TODO(), data, opts...)
}

func (p *PubSub) nextSeqno() []byte {
	seqno := make([]byte, 8)
	counter := atomic.AddUint64(&p.counter, 1)
	binary.BigEndian.PutUint64(seqno, counter)
	return seqno
}

type listPeerReq struct {
	resp  chan []peer.ID
	topic string
}

// ListPeers returns a list of peers we are connected to in the given topic.
func (p *PubSub) ListPeers(topic string) []peer.ID {
	out := make(chan []peer.ID)
	select {
	case p.getPeers <- &listPeerReq{
		resp:  out,
		topic: topic,
	}:
	case <-p.ctx.Done():
		return nil
	}
	return <-out
}

// BlacklistPeer blacklists a peer; all messages from this peer will be unconditionally dropped.
func (p *PubSub) BlacklistPeer(pid peer.ID) {
	select {
	case p.blacklistPeer <- pid:
	case <-p.ctx.Done():
	}
}

// RegisterTopicValidator registers a validator for topic.
// By default validators are asynchronous, which means they will run in a separate goroutine.
// The number of active goroutines is controlled by global and per topic validator
// throttles; if it exceeds the throttle threshold, messages will be dropped.
func (p *PubSub) RegisterTopicValidator(topic string, val Validator, opts ...ValidatorOpt) error {
	addVal := &addValReq{
		topic:    topic,
		validate: val,
		resp:     make(chan error, 1),
	}

	for _, opt := range opts {
		err := opt(addVal)
		if err != nil {
			return err
		}
	}

	select {
	case p.addVal <- addVal:
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	return <-addVal.resp
}

// UnregisterTopicValidator removes a validator from a topic.
// Returns an error if there was no validator registered with the topic.
func (p *PubSub) UnregisterTopicValidator(topic string) error {
	rmVal := &rmValReq{
		topic: topic,
		resp:  make(chan error, 1),
	}

	select {
	case p.rmVal <- rmVal:
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	return <-rmVal.resp
}
