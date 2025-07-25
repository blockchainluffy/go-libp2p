package identify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/record"
	"github.com/libp2p/go-libp2p/p2p/host/eventbus"
	useragent "github.com/libp2p/go-libp2p/p2p/protocol/identify/internal/user-agent"
	"github.com/libp2p/go-libp2p/p2p/protocol/identify/pb"
	"github.com/libp2p/go-libp2p/x/rate"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-msgio/pbio"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	msmux "github.com/multiformats/go-multistream"
	"google.golang.org/protobuf/proto"
)

var log = logging.Logger("net/identify")

const (
	// ID is the protocol.ID of version 1.0.0 of the identify service.
	ID = "/ipfs/id/1.0.0"
	// IDPush is the protocol.ID of the Identify push protocol.
	// It sends full identify messages containing the current state of the peer.
	IDPush = "/ipfs/id/push/1.0.0"
	// DefaultTimeout for all id interactions, incoming / outgoing, id / id-push.
	DefaultTimeout = 5 * time.Second
	// ServiceName is the default identify service name
	ServiceName = "libp2p.identify"

	legacyIDSize          = 2 * 1024
	signedIDSize          = 8 * 1024
	maxOwnIdentifyMsgSize = 4 * 1024 // smaller than what we accept. This is 4k to be compatible with rust-libp2p
	maxMessages           = 10
	maxPushConcurrency    = 32
	// number of addresses to keep for peers we have disconnected from for peerstore.RecentlyConnectedTTL time
	// This number can be small as we already filter peer addresses based on whether the peer is connected to us over
	// localhost, private IP or public IP address
	recentlyConnectedPeerMaxAddrs = 20
	connectedPeerMaxAddrs         = 500
)

var (
	defaultNetworkPrefixRateLimits = []rate.PrefixLimit{
		{Prefix: netip.MustParsePrefix("127.0.0.0/8"), Limit: rate.Limit{}}, // inf
		{Prefix: netip.MustParsePrefix("::1/128"), Limit: rate.Limit{}},     // inf
	}
	defaultGlobalRateLimit      = rate.Limit{RPS: 2000, Burst: 3000}
	defaultIPv4SubnetRateLimits = []rate.SubnetLimit{
		{PrefixLength: 24, Limit: rate.Limit{RPS: 0.2, Burst: 10}}, // 1 every 5 seconds
	}
	defaultIPv6SubnetRateLimits = []rate.SubnetLimit{
		{PrefixLength: 56, Limit: rate.Limit{RPS: 0.2, Burst: 10}}, // 1 every 5 seconds
		{PrefixLength: 48, Limit: rate.Limit{RPS: 0.5, Burst: 20}}, // 1 every 2 seconds
	}
)

type identifySnapshot struct {
	seq       uint64
	protocols []protocol.ID
	addrs     []ma.Multiaddr
	record    *record.Envelope
}

// Equal says if two snapshots are identical.
// It does NOT compare the sequence number.
func (s identifySnapshot) Equal(other *identifySnapshot) bool {
	hasRecord := s.record != nil
	otherHasRecord := other.record != nil
	if hasRecord != otherHasRecord {
		return false
	}
	if hasRecord && !s.record.Equal(other.record) {
		return false
	}
	if !slices.Equal(s.protocols, other.protocols) {
		return false
	}
	if len(s.addrs) != len(other.addrs) {
		return false
	}
	for i, a := range s.addrs {
		if !a.Equal(other.addrs[i]) {
			return false
		}
	}
	return true
}

type IDService interface {
	// IdentifyConn synchronously triggers an identify request on the connection and
	// waits for it to complete. If the connection is being identified by another
	// caller, this call will wait. If the connection has already been identified,
	// it will return immediately.
	IdentifyConn(network.Conn)
	// IdentifyWait triggers an identify (if the connection has not already been
	// identified) and returns a channel that is closed when the identify protocol
	// completes.
	IdentifyWait(network.Conn) <-chan struct{}
	// OwnObservedAddrs returns the addresses peers have reported we've dialed from
	OwnObservedAddrs() []ma.Multiaddr
	// ObservedAddrsFor returns the addresses peers have reported we've dialed from,
	// for a specific local address.
	ObservedAddrsFor(local ma.Multiaddr) []ma.Multiaddr
	Start()
	io.Closer
}

type identifyPushSupport uint8

const (
	identifyPushSupportUnknown identifyPushSupport = iota
	identifyPushSupported
	identifyPushUnsupported
)

type entry struct {
	// The IdentifyWaitChan is created when IdentifyWait is called for the first time.
	// IdentifyWait closes this channel when the Identify request completes, or when it fails.
	IdentifyWaitChan chan struct{}

	// PushSupport saves our knowledge about the peer's support of the Identify Push protocol.
	// Before the identify request returns, we don't know yet if the peer supports Identify Push.
	PushSupport identifyPushSupport
	// Sequence is the sequence number of the last snapshot we sent to this peer.
	Sequence uint64
}

// idService is a structure that implements ProtocolIdentify.
// It is a trivial service that gives the other peer some
// useful information about the local peer. A sort of hello.
//
// The idService sends:
//   - Our libp2p Protocol Version
//   - Our libp2p Agent Version
//   - Our public Listen Addresses
type idService struct {
	Host            host.Host
	UserAgent       string
	ProtocolVersion string

	metricsTracer MetricsTracer

	setupCompleted chan struct{} // is closed when Start has finished setting up
	ctx            context.Context
	ctxCancel      context.CancelFunc
	// track resources that need to be shut down before we shut down
	refCount sync.WaitGroup

	disableSignedPeerRecord bool
	timeout                 time.Duration

	connsMu sync.RWMutex
	// The conns map contains all connections we're currently handling.
	// Connections are inserted as soon as they're available in the swarm
	// Connections are removed from the map when the connection disconnects.
	conns map[network.Conn]entry

	addrMu sync.Mutex

	// our own observed addresses.
	observedAddrMgr            *ObservedAddrManager
	disableObservedAddrManager bool

	emitters struct {
		evtPeerProtocolsUpdated        event.Emitter
		evtPeerIdentificationCompleted event.Emitter
		evtPeerIdentificationFailed    event.Emitter
	}

	currentSnapshot struct {
		sync.Mutex
		snapshot identifySnapshot
	}

	natEmitter *natEmitter

	rateLimiter *rate.Limiter
}

type normalizer interface {
	NormalizeMultiaddr(ma.Multiaddr) ma.Multiaddr
}

// NewIDService constructs a new *idService and activates it by
// attaching its stream handler to the given host.Host.
func NewIDService(h host.Host, opts ...Option) (*idService, error) {
	cfg := config{
		timeout: DefaultTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	userAgent := useragent.DefaultUserAgent()
	if cfg.userAgent != "" {
		userAgent = cfg.userAgent
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &idService{
		Host:                    h,
		UserAgent:               userAgent,
		ProtocolVersion:         cfg.protocolVersion,
		ctx:                     ctx,
		ctxCancel:               cancel,
		conns:                   make(map[network.Conn]entry),
		disableSignedPeerRecord: cfg.disableSignedPeerRecord,
		setupCompleted:          make(chan struct{}),
		metricsTracer:           cfg.metricsTracer,
		timeout:                 cfg.timeout,
		rateLimiter: &rate.Limiter{
			GlobalLimit:         defaultGlobalRateLimit,
			NetworkPrefixLimits: defaultNetworkPrefixRateLimits,
			SubnetRateLimiter: rate.SubnetLimiter{
				IPv4SubnetLimits: defaultIPv4SubnetRateLimits,
				IPv6SubnetLimits: defaultIPv6SubnetRateLimits,
				GracePeriod:      1 * time.Minute,
			},
		},
	}

	var normalize func(ma.Multiaddr) ma.Multiaddr
	if hn, ok := h.(normalizer); ok {
		normalize = hn.NormalizeMultiaddr
	}

	var err error
	if cfg.disableObservedAddrManager {
		s.disableObservedAddrManager = true
	} else {
		observedAddrs, err := NewObservedAddrManager(h.Network().ListenAddresses,
			h.Addrs, h.Network().InterfaceListenAddresses, normalize)
		if err != nil {
			return nil, fmt.Errorf("failed to create observed address manager: %s", err)
		}
		natEmitter, err := newNATEmitter(h, observedAddrs, time.Minute)
		if err != nil {
			return nil, fmt.Errorf("failed to create nat emitter: %s", err)
		}
		s.natEmitter = natEmitter
		s.observedAddrMgr = observedAddrs
	}

	s.emitters.evtPeerProtocolsUpdated, err = h.EventBus().Emitter(&event.EvtPeerProtocolsUpdated{})
	if err != nil {
		log.Warnf("identify service not emitting peer protocol updates; err: %s", err)
	}
	s.emitters.evtPeerIdentificationCompleted, err = h.EventBus().Emitter(&event.EvtPeerIdentificationCompleted{})
	if err != nil {
		log.Warnf("identify service not emitting identification completed events; err: %s", err)
	}
	s.emitters.evtPeerIdentificationFailed, err = h.EventBus().Emitter(&event.EvtPeerIdentificationFailed{})
	if err != nil {
		log.Warnf("identify service not emitting identification failed events; err: %s", err)
	}
	return s, nil
}

func (ids *idService) Start() {
	ids.Host.Network().Notify((*netNotifiee)(ids))
	ids.Host.SetStreamHandler(ID, ids.handleIdentifyRequest)
	ids.Host.SetStreamHandler(IDPush, ids.rateLimiter.Limit(ids.handlePush))
	ids.updateSnapshot()
	close(ids.setupCompleted)

	ids.refCount.Add(1)
	go ids.loop(ids.ctx)
}

func (ids *idService) loop(ctx context.Context) {
	defer ids.refCount.Done()

	sub, err := ids.Host.EventBus().Subscribe(
		[]any{&event.EvtLocalProtocolsUpdated{}, &event.EvtLocalAddressesUpdated{}},
		eventbus.BufSize(256),
		eventbus.Name("identify (loop)"),
	)
	if err != nil {
		log.Errorf("failed to subscribe to events on the bus, err=%s", err)
		return
	}
	defer sub.Close()

	// Send pushes from a separate Go routine.
	// That way, we can end up with
	// * this Go routine busy looping over all peers in sendPushes
	// * another push being queued in the triggerPush channel
	triggerPush := make(chan struct{}, 1)
	ids.refCount.Add(1)
	go func() {
		defer ids.refCount.Done()

		for {
			select {
			case <-ctx.Done():
				return
			case <-triggerPush:
				ids.sendPushes(ctx)
			}
		}
	}()

	for {
		select {
		case e, ok := <-sub.Out():
			if !ok {
				return
			}
			if updated := ids.updateSnapshot(); !updated {
				continue
			}
			if ids.metricsTracer != nil {
				ids.metricsTracer.TriggeredPushes(e)
			}
			select {
			case triggerPush <- struct{}{}:
			default: // we already have one more push queued, no need to queue another one
			}
		case <-ctx.Done():
			return
		}
	}
}

func (ids *idService) sendPushes(ctx context.Context) {
	ids.connsMu.RLock()
	conns := make([]network.Conn, 0, len(ids.conns))
	for c, e := range ids.conns {
		// Push even if we don't know if push is supported.
		// This will be only the case while the IdentifyWaitChan call is in flight.
		if e.PushSupport == identifyPushSupported || e.PushSupport == identifyPushSupportUnknown {
			conns = append(conns, c)
		}
	}
	ids.connsMu.RUnlock()

	sem := make(chan struct{}, maxPushConcurrency)
	var wg sync.WaitGroup
	for _, c := range conns {
		// check if the connection is still alive
		ids.connsMu.RLock()
		e, ok := ids.conns[c]
		ids.connsMu.RUnlock()
		if !ok {
			continue
		}
		// check if we already sent the current snapshot to this peer
		ids.currentSnapshot.Lock()
		snapshot := ids.currentSnapshot.snapshot
		ids.currentSnapshot.Unlock()
		if e.Sequence >= snapshot.seq {
			log.Debugw("already sent this snapshot to peer", "peer", c.RemotePeer(), "seq", snapshot.seq)
			continue
		}
		// we haven't, send it now
		sem <- struct{}{}
		wg.Add(1)
		go func(c network.Conn) {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(ctx, ids.timeout)
			defer cancel()

			str, err := newStreamAndNegotiate(ctx, c, IDPush, ids.timeout)
			if err != nil { // connection might have been closed recently
				return
			}
			// TODO: find out if the peer supports push if we didn't have any information about push support
			if err := ids.sendIdentifyResp(str, true); err != nil {
				log.Debugw("failed to send identify push", "peer", c.RemotePeer(), "error", err)
				return
			}
		}(c)
	}
	wg.Wait()
}

// Close shuts down the idService
func (ids *idService) Close() error {
	ids.ctxCancel()
	if !ids.disableObservedAddrManager {
		ids.observedAddrMgr.Close()
		ids.natEmitter.Close()
	}
	ids.refCount.Wait()
	return nil
}

func (ids *idService) OwnObservedAddrs() []ma.Multiaddr {
	if ids.disableObservedAddrManager {
		return nil
	}
	return ids.observedAddrMgr.Addrs()
}

func (ids *idService) ObservedAddrsFor(local ma.Multiaddr) []ma.Multiaddr {
	if ids.disableObservedAddrManager {
		return nil
	}
	return ids.observedAddrMgr.AddrsFor(local)
}

// IdentifyConn runs the Identify protocol on a connection.
// It returns when we've received the peer's Identify message (or the request fails).
// If successful, the peer store will contain the peer's addresses and supported protocols.
func (ids *idService) IdentifyConn(c network.Conn) {
	<-ids.IdentifyWait(c)
}

// IdentifyWait runs the Identify protocol on a connection.
// It doesn't block and returns a channel that is closed when we receive
// the peer's Identify message (or the request fails).
// If successful, the peer store will contain the peer's addresses and supported protocols.
func (ids *idService) IdentifyWait(c network.Conn) <-chan struct{} {
	ids.connsMu.Lock()
	defer ids.connsMu.Unlock()

	e, found := ids.conns[c]
	if !found {
		// No entry found. We may have gotten an out of order notification. Check it we should have this conn (because we're still connected)
		// We hold the ids.connsMu lock so this is safe since a disconnect event will be processed later if we are connected.
		if c.IsClosed() {
			log.Debugw("connection not found in identify service", "peer", c.RemotePeer())
			ch := make(chan struct{})
			close(ch)
			return ch
		} else {
			ids.addConnWithLock(c)
		}
	}

	if e.IdentifyWaitChan != nil {
		return e.IdentifyWaitChan
	}
	// First call to IdentifyWait for this connection. Create the channel.
	e.IdentifyWaitChan = make(chan struct{})
	ids.conns[c] = e

	// Spawn an identify. The connection may actually be closed
	// already, but that doesn't really matter. We'll fail to open a
	// stream then forget the connection.
	go func() {
		defer close(e.IdentifyWaitChan)
		if err := ids.identifyConn(c); err != nil {
			log.Warnf("failed to identify %s: %s", c.RemotePeer(), err)
			ids.emitters.evtPeerIdentificationFailed.Emit(event.EvtPeerIdentificationFailed{Peer: c.RemotePeer(), Reason: err})
			return
		}
	}()

	return e.IdentifyWaitChan
}

// newStreamAndNegotiate opens a new stream on the given connection and negotiates the given protocol.
func newStreamAndNegotiate(ctx context.Context, c network.Conn, proto protocol.ID, timeout time.Duration) (network.Stream, error) {
	s, err := c.NewStream(network.WithAllowLimitedConn(ctx, "identify"))
	if err != nil {
		log.Debugw("error opening identify stream", "peer", c.RemotePeer(), "error", err)
		return nil, fmt.Errorf("failed to open new stream: %w", err)
	}

	// Ignore the error. Consistent with our previous behavior. (See https://github.com/libp2p/go-libp2p/issues/3109)
	_ = s.SetDeadline(time.Now().Add(timeout))

	if err := s.SetProtocol(proto); err != nil {
		log.Warnf("error setting identify protocol for stream: %s", err)
		_ = s.Reset()
		return nil, fmt.Errorf("failed to set protocol: %w", err)
	}

	// ok give the response to our handler.
	if err := msmux.SelectProtoOrFail(proto, s); err != nil {
		log.Infow("failed negotiate identify protocol with peer", "peer", c.RemotePeer(), "error", err)
		_ = s.Reset()
		return nil, fmt.Errorf("multistream mux select protocol failed: %w", err)
	}
	return s, nil
}

func (ids *idService) identifyConn(c network.Conn) error {
	ctx, cancel := context.WithTimeout(ids.ctx, ids.timeout)
	defer cancel()
	s, err := newStreamAndNegotiate(network.WithAllowLimitedConn(ctx, "identify"), c, ID, ids.timeout)
	if err != nil {
		log.Debugw("error opening identify stream", "peer", c.RemotePeer(), "error", err)
		return err
	}

	return ids.handleIdentifyResponse(s, false)
}

// handlePush handles incoming identify push streams
func (ids *idService) handlePush(s network.Stream) {
	s.SetDeadline(time.Now().Add(ids.timeout))
	if err := ids.handleIdentifyResponse(s, true); err != nil {
		log.Debugf("failed to handle identify push: %s", err)
	}
}

func (ids *idService) handleIdentifyRequest(s network.Stream) {
	_ = ids.sendIdentifyResp(s, false)
}

func (ids *idService) sendIdentifyResp(s network.Stream, isPush bool) error {
	if err := s.Scope().SetService(ServiceName); err != nil {
		s.Reset()
		return fmt.Errorf("failed to attaching stream to identify service: %w", err)
	}
	defer s.Close()

	ids.currentSnapshot.Lock()
	snapshot := ids.currentSnapshot.snapshot
	ids.currentSnapshot.Unlock()

	log.Debugw("sending snapshot", "seq", snapshot.seq, "protocols", snapshot.protocols, "addrs", snapshot.addrs)

	mes := ids.createBaseIdentifyResponse(s.Conn(), &snapshot)
	mes.SignedPeerRecord = ids.getSignedRecord(&snapshot)

	log.Debugf("%s sending message to %s %s", ID, s.Conn().RemotePeer(), s.Conn().RemoteMultiaddr())
	if err := ids.writeChunkedIdentifyMsg(s, mes); err != nil {
		return err
	}

	if ids.metricsTracer != nil {
		ids.metricsTracer.IdentifySent(isPush, len(mes.Protocols), len(mes.ListenAddrs))
	}

	ids.connsMu.Lock()
	defer ids.connsMu.Unlock()
	e, ok := ids.conns[s.Conn()]
	// The connection might already have been closed.
	// We *should* receive the Connected notification from the swarm before we're able to accept the peer's
	// Identify stream, but if that for some reason doesn't work, we also wouldn't have a map entry here.
	// The only consequence would be that we send a spurious Push to that peer later.
	if !ok {
		return nil
	}
	e.Sequence = snapshot.seq
	ids.conns[s.Conn()] = e
	return nil
}

func (ids *idService) handleIdentifyResponse(s network.Stream, isPush bool) error {
	if err := s.Scope().SetService(ServiceName); err != nil {
		log.Warnf("error attaching stream to identify service: %s", err)
		s.Reset()
		return err
	}

	if err := s.Scope().ReserveMemory(signedIDSize, network.ReservationPriorityAlways); err != nil {
		log.Warnf("error reserving memory for identify stream: %s", err)
		s.Reset()
		return err
	}
	defer s.Scope().ReleaseMemory(signedIDSize)

	c := s.Conn()

	r := pbio.NewDelimitedReader(s, signedIDSize)
	mes := &pb.Identify{}

	if err := readAllIDMessages(r, mes); err != nil {
		log.Warn("error reading identify message: ", err)
		s.Reset()
		return err
	}

	defer s.Close()

	log.Debugf("%s received message from %s %s", s.Protocol(), c.RemotePeer(), c.RemoteMultiaddr())

	ids.consumeMessage(mes, c, isPush)

	if ids.metricsTracer != nil {
		ids.metricsTracer.IdentifyReceived(isPush, len(mes.Protocols), len(mes.ListenAddrs))
	}

	ids.connsMu.Lock()
	defer ids.connsMu.Unlock()
	e, ok := ids.conns[c]
	if !ok { // might already have disconnected
		return nil
	}
	sup, err := ids.Host.Peerstore().SupportsProtocols(c.RemotePeer(), IDPush)
	if supportsIdentifyPush := err == nil && len(sup) > 0; supportsIdentifyPush {
		e.PushSupport = identifyPushSupported
	} else {
		e.PushSupport = identifyPushUnsupported
	}

	if ids.metricsTracer != nil {
		ids.metricsTracer.ConnPushSupport(e.PushSupport)
	}

	ids.conns[c] = e
	return nil
}

func readAllIDMessages(r pbio.Reader, finalMsg proto.Message) error {
	mes := &pb.Identify{}
	for i := 0; i < maxMessages; i++ {
		switch err := r.ReadMsg(mes); err {
		case io.EOF:
			return nil
		case nil:
			proto.Merge(finalMsg, mes)
		default:
			return err
		}
	}

	return fmt.Errorf("too many parts")
}

func (ids *idService) updateSnapshot() (updated bool) {
	protos := ids.Host.Mux().Protocols()
	slices.Sort(protos)

	addrs := ids.Host.Addrs()
	slices.SortFunc(addrs, func(a, b ma.Multiaddr) int { return bytes.Compare(a.Bytes(), b.Bytes()) })

	usedSpace := len(ids.ProtocolVersion) + len(ids.UserAgent)
	for i := 0; i < len(protos); i++ {
		usedSpace += len(protos[i])
	}
	addrs = trimHostAddrList(addrs, maxOwnIdentifyMsgSize-usedSpace-256) // 256 bytes of buffer

	snapshot := identifySnapshot{
		addrs:     addrs,
		protocols: protos,
	}

	if !ids.disableSignedPeerRecord {
		if cab, ok := peerstore.GetCertifiedAddrBook(ids.Host.Peerstore()); ok {
			snapshot.record = cab.GetPeerRecord(ids.Host.ID())
		}
	}

	ids.currentSnapshot.Lock()
	defer ids.currentSnapshot.Unlock()

	if ids.currentSnapshot.snapshot.Equal(&snapshot) {
		return false
	}

	snapshot.seq = ids.currentSnapshot.snapshot.seq + 1
	ids.currentSnapshot.snapshot = snapshot

	log.Debugw("updating snapshot", "seq", snapshot.seq, "addrs", snapshot.addrs)
	return true
}

func (ids *idService) writeChunkedIdentifyMsg(s network.Stream, mes *pb.Identify) error {
	writer := pbio.NewDelimitedWriter(s)

	if mes.SignedPeerRecord == nil || proto.Size(mes) <= legacyIDSize {
		return writer.WriteMsg(mes)
	}

	sr := mes.SignedPeerRecord
	mes.SignedPeerRecord = nil
	if err := writer.WriteMsg(mes); err != nil {
		return err
	}
	// then write just the signed record
	return writer.WriteMsg(&pb.Identify{SignedPeerRecord: sr})
}

func (ids *idService) createBaseIdentifyResponse(conn network.Conn, snapshot *identifySnapshot) *pb.Identify {
	mes := &pb.Identify{}

	remoteAddr := conn.RemoteMultiaddr()
	localAddr := conn.LocalMultiaddr()

	// set protocols this node is currently handling
	mes.Protocols = protocol.ConvertToStrings(snapshot.protocols)

	// observed address so other side is informed of their
	// "public" address, at least in relation to us.
	mes.ObservedAddr = remoteAddr.Bytes()

	// populate unsigned addresses.
	// peers that do not yet support signed addresses will need this.
	// Note: LocalMultiaddr is sometimes 0.0.0.0
	viaLoopback := manet.IsIPLoopback(localAddr) || manet.IsIPLoopback(remoteAddr)
	mes.ListenAddrs = make([][]byte, 0, len(snapshot.addrs))
	for _, addr := range snapshot.addrs {
		if !viaLoopback && manet.IsIPLoopback(addr) {
			continue
		}
		mes.ListenAddrs = append(mes.ListenAddrs, addr.Bytes())
	}
	// set our public key
	ownKey := ids.Host.Peerstore().PubKey(ids.Host.ID())

	// check if we even have a public key.
	if ownKey == nil {
		// public key is nil. We are either using insecure transport or something erratic happened.
		// check if we're even operating in "secure mode"
		if ids.Host.Peerstore().PrivKey(ids.Host.ID()) != nil {
			// private key is present. But NO public key. Something bad happened.
			log.Errorf("did not have own public key in Peerstore")
		}
		// if neither of the key is present it is safe to assume that we are using an insecure transport.
	} else {
		// public key is present. Safe to proceed.
		if kb, err := crypto.MarshalPublicKey(ownKey); err != nil {
			log.Errorf("failed to convert key to bytes")
		} else {
			mes.PublicKey = kb
		}
	}

	// set protocol versions
	mes.ProtocolVersion = &ids.ProtocolVersion
	mes.AgentVersion = &ids.UserAgent

	return mes
}

func (ids *idService) getSignedRecord(snapshot *identifySnapshot) []byte {
	if ids.disableSignedPeerRecord || snapshot.record == nil {
		return nil
	}

	recBytes, err := snapshot.record.Marshal()
	if err != nil {
		log.Errorw("failed to marshal signed record", "err", err)
		return nil
	}

	return recBytes
}

// diff takes two slices of strings (a and b) and computes which elements were added and removed in b
func diff(a, b []protocol.ID) (added, removed []protocol.ID) {
	// This is O(n^2), but it's fine because the slices are small.
	for _, x := range b {
		var found bool
		for _, y := range a {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			added = append(added, x)
		}
	}
	for _, x := range a {
		var found bool
		for _, y := range b {
			if x == y {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, x)
		}
	}
	return
}

func (ids *idService) consumeMessage(mes *pb.Identify, c network.Conn, isPush bool) {
	p := c.RemotePeer()

	supported, _ := ids.Host.Peerstore().GetProtocols(p)
	mesProtocols := protocol.ConvertFromStrings(mes.Protocols)
	added, removed := diff(supported, mesProtocols)
	ids.Host.Peerstore().SetProtocols(p, mesProtocols...)
	if isPush {
		ids.emitters.evtPeerProtocolsUpdated.Emit(event.EvtPeerProtocolsUpdated{
			Peer:    p,
			Added:   added,
			Removed: removed,
		})
	}

	obsAddr, err := ma.NewMultiaddrBytes(mes.GetObservedAddr())
	if err != nil {
		log.Debugf("error parsing received observed addr for %s: %s", c, err)
		obsAddr = nil
	}

	if obsAddr != nil && !ids.disableObservedAddrManager {
		// TODO refactor this to use the emitted events instead of having this func call explicitly.
		ids.observedAddrMgr.Record(c, obsAddr)
	}

	// mes.ListenAddrs
	laddrs := mes.GetListenAddrs()
	lmaddrs := make([]ma.Multiaddr, 0, len(laddrs))
	for _, addr := range laddrs {
		maddr, err := ma.NewMultiaddrBytes(addr)
		if err != nil {
			log.Debugf("%s failed to parse multiaddr from %s %s", ID,
				p, c.RemoteMultiaddr())
			continue
		}
		lmaddrs = append(lmaddrs, maddr)
	}

	// NOTE: Do not add `c.RemoteMultiaddr()` to the peerstore if the remote
	// peer doesn't tell us to do so. Otherwise, we'll advertise it.
	//
	// This can cause an "addr-splosion" issue where the network will slowly
	// gossip and collect observed but unadvertised addresses. Given a NAT
	// that picks random source ports, this can cause DHT nodes to collect
	// many undialable addresses for other peers.

	// add certified addresses for the peer, if they sent us a signed peer record
	// otherwise use the unsigned addresses.
	signedPeerRecord, err := signedPeerRecordFromMessage(mes)
	if err != nil {
		log.Debugf("error getting peer record from Identify message: %v", err)
	}

	// Extend the TTLs on the known (probably) good addresses.
	// Taking the lock ensures that we don't concurrently process a disconnect.
	ids.addrMu.Lock()
	ttl := peerstore.RecentlyConnectedAddrTTL
	switch ids.Host.Network().Connectedness(p) {
	case network.Limited, network.Connected:
		ttl = peerstore.ConnectedAddrTTL
	}

	// Downgrade connected and recently connected addrs to a temporary TTL.
	for _, ttl := range []time.Duration{
		peerstore.RecentlyConnectedAddrTTL,
		peerstore.ConnectedAddrTTL,
	} {
		ids.Host.Peerstore().UpdateAddrs(p, ttl, peerstore.TempAddrTTL)
	}

	var addrs []ma.Multiaddr
	if signedPeerRecord != nil {
		signedAddrs, err := ids.consumeSignedPeerRecord(c.RemotePeer(), signedPeerRecord)
		if err != nil {
			log.Debugf("failed to consume signed peer record: %s", err)
			signedPeerRecord = nil
		} else {
			addrs = signedAddrs
		}
	} else {
		addrs = lmaddrs
	}
	addrs = filterAddrs(addrs, c.RemoteMultiaddr())
	if len(addrs) > connectedPeerMaxAddrs {
		addrs = addrs[:connectedPeerMaxAddrs]
	}

	ids.Host.Peerstore().AddAddrs(p, addrs, ttl)

	// Finally, expire all temporary addrs.
	ids.Host.Peerstore().UpdateAddrs(p, peerstore.TempAddrTTL, 0)
	ids.addrMu.Unlock()

	log.Debugf("%s received listen addrs for %s: %s", c.LocalPeer(), c.RemotePeer(), addrs)

	// get protocol versions
	pv := mes.GetProtocolVersion()
	av := mes.GetAgentVersion()

	ids.Host.Peerstore().Put(p, "ProtocolVersion", pv)
	ids.Host.Peerstore().Put(p, "AgentVersion", av)

	// get the key from the other side. we may not have it (no-auth transport)
	ids.consumeReceivedPubKey(c, mes.PublicKey)

	ids.emitters.evtPeerIdentificationCompleted.Emit(event.EvtPeerIdentificationCompleted{
		Peer:             c.RemotePeer(),
		Conn:             c,
		ListenAddrs:      lmaddrs,
		Protocols:        mesProtocols,
		SignedPeerRecord: signedPeerRecord,
		ObservedAddr:     obsAddr,
		ProtocolVersion:  pv,
		AgentVersion:     av,
	})
}

func (ids *idService) consumeSignedPeerRecord(p peer.ID, signedPeerRecord *record.Envelope) ([]ma.Multiaddr, error) {
	if signedPeerRecord.PublicKey == nil {
		return nil, errors.New("missing pubkey")
	}
	id, err := peer.IDFromPublicKey(signedPeerRecord.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive peer ID: %s", err)
	}
	if id != p {
		return nil, fmt.Errorf("received signed peer record envelope for unexpected peer ID. expected %s, got %s", p, id)
	}
	r, err := signedPeerRecord.Record()
	if err != nil {
		return nil, fmt.Errorf("failed to obtain record: %w", err)
	}
	rec, ok := r.(*peer.PeerRecord)
	if !ok {
		return nil, errors.New("not a peer record")
	}
	if rec.PeerID != p {
		return nil, fmt.Errorf("received signed peer record for unexpected peer ID. expected %s, got %s", p, rec.PeerID)
	}
	// Don't put the signed peer record into the peer store.
	// They're not used anywhere.
	// All we care about are the addresses.
	return rec.Addrs, nil
}

func (ids *idService) consumeReceivedPubKey(c network.Conn, kb []byte) {
	lp := c.LocalPeer()
	rp := c.RemotePeer()

	if kb == nil {
		log.Debugf("%s did not receive public key for remote peer: %s", lp, rp)
		return
	}

	newKey, err := crypto.UnmarshalPublicKey(kb)
	if err != nil {
		log.Warnf("%s cannot unmarshal key from remote peer: %s, %s", lp, rp, err)
		return
	}

	// verify key matches peer.ID
	np, err := peer.IDFromPublicKey(newKey)
	if err != nil {
		log.Debugf("%s cannot get peer.ID from key of remote peer: %s, %s", lp, rp, err)
		return
	}

	if np != rp {
		// if the newKey's peer.ID does not match known peer.ID...

		if rp == "" && np != "" {
			// if local peerid is empty, then use the new, sent key.
			err := ids.Host.Peerstore().AddPubKey(rp, newKey)
			if err != nil {
				log.Debugf("%s could not add key for %s to peerstore: %s", lp, rp, err)
			}

		} else {
			// we have a local peer.ID and it does not match the sent key... error.
			log.Errorf("%s received key for remote peer %s mismatch: %s", lp, rp, np)
		}
		return
	}

	currKey := ids.Host.Peerstore().PubKey(rp)
	if currKey == nil {
		// no key? no auth transport. set this one.
		err := ids.Host.Peerstore().AddPubKey(rp, newKey)
		if err != nil {
			log.Debugf("%s could not add key for %s to peerstore: %s", lp, rp, err)
		}
		return
	}

	// ok, we have a local key, we should verify they match.
	if currKey.Equals(newKey) {
		return // ok great. we're done.
	}

	// weird, got a different key... but the different key MATCHES the peer.ID.
	// this odd. let's log error and investigate. this should basically never happen
	// and it means we have something funky going on and possibly a bug.
	log.Errorf("%s identify got a different key for: %s", lp, rp)

	// okay... does ours NOT match the remote peer.ID?
	cp, err := peer.IDFromPublicKey(currKey)
	if err != nil {
		log.Errorf("%s cannot get peer.ID from local key of remote peer: %s, %s", lp, rp, err)
		return
	}
	if cp != rp {
		log.Errorf("%s local key for remote peer %s yields different peer.ID: %s", lp, rp, cp)
		return
	}

	// okay... curr key DOES NOT match new key. both match peer.ID. wat?
	log.Errorf("%s local key and received key for %s do not match, but match peer.ID", lp, rp)
}

// HasConsistentTransport returns true if the address 'a' shares a
// protocol set with any address in the green set. This is used
// to check if a given address might be one of the addresses a peer is
// listening on.
func HasConsistentTransport(a ma.Multiaddr, green []ma.Multiaddr) bool {
	protosMatch := func(a, b []ma.Protocol) bool {
		if len(a) != len(b) {
			return false
		}

		for i, p := range a {
			if b[i].Code != p.Code {
				return false
			}
		}
		return true
	}

	protos := a.Protocols()

	for _, ga := range green {
		if protosMatch(protos, ga.Protocols()) {
			return true
		}
	}

	return false
}

// addConnWithLock assuems caller holds the connsMu lock
func (ids *idService) addConnWithLock(c network.Conn) {
	_, found := ids.conns[c]
	if !found {
		<-ids.setupCompleted
		ids.conns[c] = entry{}
	}
}

func signedPeerRecordFromMessage(msg *pb.Identify) (*record.Envelope, error) {
	if len(msg.SignedPeerRecord) == 0 {
		return nil, nil
	}
	env, _, err := record.ConsumeEnvelope(msg.SignedPeerRecord, peer.PeerRecordEnvelopeDomain)
	return env, err
}

// netNotifiee defines methods to be used with the swarm
type netNotifiee idService

func (nn *netNotifiee) IDService() *idService {
	return (*idService)(nn)
}

func (nn *netNotifiee) Connected(_ network.Network, c network.Conn) {
	ids := nn.IDService()

	ids.connsMu.Lock()
	ids.addConnWithLock(c)
	ids.connsMu.Unlock()

	nn.IDService().IdentifyWait(c)
}

func (nn *netNotifiee) Disconnected(_ network.Network, c network.Conn) {
	ids := nn.IDService()

	// Stop tracking the connection.
	ids.connsMu.Lock()
	delete(ids.conns, c)
	ids.connsMu.Unlock()

	if !ids.disableObservedAddrManager {
		ids.observedAddrMgr.removeConn(c)
	}

	// Last disconnect.
	// Undo the setting of addresses to peer.ConnectedAddrTTL we did
	ids.addrMu.Lock()
	defer ids.addrMu.Unlock()

	// This check MUST happen after acquiring the Lock as identify on a different connection
	// might be trying to add addresses.
	switch ids.Host.Network().Connectedness(c.RemotePeer()) {
	case network.Connected, network.Limited:
		return
	}
	// peerstore returns the elements in a random order as it uses a map to store the addresses
	addrs := ids.Host.Peerstore().Addrs(c.RemotePeer())
	n := len(addrs)
	if n > recentlyConnectedPeerMaxAddrs {
		// We want to always save the address we are connected to
		for i, a := range addrs {
			if a.Equal(c.RemoteMultiaddr()) {
				addrs[i], addrs[0] = addrs[0], addrs[i]
			}
		}
		n = recentlyConnectedPeerMaxAddrs
	}
	ids.Host.Peerstore().UpdateAddrs(c.RemotePeer(), peerstore.ConnectedAddrTTL, peerstore.TempAddrTTL)
	ids.Host.Peerstore().AddAddrs(c.RemotePeer(), addrs[:n], peerstore.RecentlyConnectedAddrTTL)
	ids.Host.Peerstore().UpdateAddrs(c.RemotePeer(), peerstore.TempAddrTTL, 0)
}

func (nn *netNotifiee) Listen(_ network.Network, _ ma.Multiaddr)      {}
func (nn *netNotifiee) ListenClose(_ network.Network, _ ma.Multiaddr) {}

// filterAddrs filters the address slice based on the remote multiaddr:
//   - if it's a localhost address, no filtering is applied
//   - if it's a private network address, all localhost addresses are filtered out
//   - if it's a public address, all non-public addresses are filtered out
//   - if none of the above, (e.g. discard prefix), no filtering is applied.
//     We can't do anything meaningful here so we do nothing.
func filterAddrs(addrs []ma.Multiaddr, remote ma.Multiaddr) []ma.Multiaddr {
	switch {
	case manet.IsIPLoopback(remote):
		return addrs
	case manet.IsPrivateAddr(remote):
		return ma.FilterAddrs(addrs, func(a ma.Multiaddr) bool { return !manet.IsIPLoopback(a) })
	case manet.IsPublicAddr(remote):
		return ma.FilterAddrs(addrs, manet.IsPublicAddr)
	default:
		return addrs
	}
}

func trimHostAddrList(addrs []ma.Multiaddr, maxSize int) []ma.Multiaddr {
	totalSize := 0
	for _, a := range addrs {
		totalSize += len(a.Bytes())
	}
	if totalSize <= maxSize {
		return addrs
	}

	score := func(addr ma.Multiaddr) int {
		var res int
		if manet.IsPublicAddr(addr) {
			res |= 1 << 12
		} else if !manet.IsIPLoopback(addr) {
			res |= 1 << 11
		}
		var protocolWeight int
		ma.ForEach(addr, func(c ma.Component) bool {
			switch c.Protocol().Code {
			case ma.P_QUIC_V1:
				protocolWeight = 5
			case ma.P_TCP:
				protocolWeight = 4
			case ma.P_WSS:
				protocolWeight = 3
			case ma.P_WEBTRANSPORT:
				protocolWeight = 2
			case ma.P_WEBRTC_DIRECT:
				protocolWeight = 1
			case ma.P_P2P:
				return false
			}
			return true
		})
		res |= 1 << protocolWeight
		return res
	}

	slices.SortStableFunc(addrs, func(a, b ma.Multiaddr) int {
		return score(b) - score(a) // b-a for reverse order
	})
	totalSize = 0
	for i, a := range addrs {
		totalSize += len(a.Bytes())
		if totalSize > maxSize {
			addrs = addrs[:i]
			break
		}
	}
	return addrs
}
