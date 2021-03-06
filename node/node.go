package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"sync"
	"time"

	chain "github.com/elastos/Elastos.ELA/blockchain"
	"github.com/elastos/Elastos.ELA/bloom"
	. "github.com/elastos/Elastos.ELA/config"
	. "github.com/elastos/Elastos.ELA/core"
	"github.com/elastos/Elastos.ELA/events"
	"github.com/elastos/Elastos.ELA/log"
	"github.com/elastos/Elastos.ELA/protocol"

	. "github.com/elastos/Elastos.ELA.Utility/common"
	"github.com/elastos/Elastos.ELA.Utility/p2p"
	"github.com/elastos/Elastos.ELA.Utility/p2p/msg"
)

var LocalNode *node

type Semaphore chan struct{}

func MakeSemaphore(n int) Semaphore {
	return make(chan struct{}, n)
}

func (s Semaphore) acquire() { s <- struct{}{} }
func (s Semaphore) release() { <-s }

type node struct {
	//sync.RWMutex	//The Lock not be used as expected to use function channel instead of lock
	p2p.PeerState          // node state
	id       uint64        // The nodes's id
	version  uint32        // The network protocol the node used
	services uint64        // The services the node supplied
	relay    bool          // The relay capability of the node (merge into capbility flag)
	height   uint64        // The node latest block height
	external bool          // Indicate if this is an external node
	txnCnt   uint64        // The transactions be transmit by this node
	rxTxnCnt uint64        // The transaction received by this node
	link                   // The link status and infomation
	neighbours             // The neighbor node connect with currently node except itself
	events   *events.Event // The event queue to notice notice other modules
	chain.TxPool           // Unconfirmed transaction pool
	idCache                // The buffer to store the id of the items which already be processed
	filter   *bloom.Filter // The bloom filter of a spv node
	/*
	 * |--|--|--|--|--|--|isSyncFailed|isSyncHeaders|
	 */
	syncFlag                 uint8
	flagLock                 sync.RWMutex
	cachelock                sync.RWMutex
	requestedBlockLock       sync.RWMutex
	nodeDisconnectSubscriber events.Subscriber
	ConnectingNodes
	KnownAddressList
	DefaultMaxPeers    uint
	headerFirstMode    bool
	RequestedBlockList map[Uint256]time.Time
	syncTimer          *syncTimer
	SyncBlkReqSem      Semaphore
	StartHash          Uint256
	StopHash           Uint256
}

type ConnectingNodes struct {
	sync.RWMutex
	List map[string]struct{}
}

func (cn *ConnectingNodes) init() {
	cn.List = make(map[string]struct{})
}

func (cn *ConnectingNodes) add(addr string) bool {
	cn.Lock()
	defer cn.Unlock()
	_, ok := cn.List[addr]
	if !ok {
		cn.List[addr] = struct{}{}
	}
	return !ok
}

func (cn *ConnectingNodes) del(addr string) {
	cn.Lock()
	defer cn.Unlock()
	delete(cn.List, addr)
}

func NewNode(magic uint32, conn net.Conn) *node {
	node := new(node)
	node.conn = conn
	node.filter = bloom.LoadFilter(nil)
	node.MsgHelper = p2p.NewMsgHelper(magic, uint32(Parameters.MaxBlockSize), conn, NewHandlerBase(node))
	runtime.SetFinalizer(node, rmNode)
	return node
}

func InitLocalNode() protocol.Noder {
	LocalNode = NewNode(Parameters.Magic, nil)
	LocalNode.version = protocol.ProtocolVersion

	LocalNode.SyncBlkReqSem = MakeSemaphore(protocol.MaxSyncHdrReq)

	LocalNode.link.port = Parameters.NodePort
	if Parameters.OpenService {
		LocalNode.services += protocol.OpenService
	}
	LocalNode.relay = true
	idHash := sha256.Sum256([]byte(strconv.Itoa(int(time.Now().UnixNano()))))
	binary.Read(bytes.NewBuffer(idHash[:8]), binary.LittleEndian, &(LocalNode.id))

	log.Info(fmt.Sprintf("Init node ID to 0x%x", LocalNode.id))
	LocalNode.neighbours.init()
	LocalNode.ConnectingNodes.init()
	LocalNode.KnownAddressList.init()
	LocalNode.TxPool.Init()
	LocalNode.events = events.NewEvent()
	LocalNode.idCache.init()
	LocalNode.nodeDisconnectSubscriber = LocalNode.Events().Subscribe(events.EventNodeDisconnect, LocalNode.NodeDisconnect)
	LocalNode.RequestedBlockList = make(map[Uint256]time.Time)
	LocalNode.handshakeQueue.init()
	LocalNode.syncTimer = newSyncTimer(LocalNode.stopSyncing)
	LocalNode.initConnection()
	go LocalNode.Start()
	go monitorNodeState()
	return LocalNode
}

func (node *node) Start() {
	node.ConnectNodes()
	node.waitForNeighbourConnections()

	ticker := time.NewTicker(time.Second * protocol.HeartbeatDuration)
	for {
		go node.ConnectNodes()
		go node.SyncBlocks()
		<-ticker.C
	}
}

func (node *node) UpdateMsgHelper(handler p2p.MsgHandler) {
	node.MsgHelper.Update(handler)
}

func (node *node) AddToConnectingList(addr string) bool {
	return node.ConnectingNodes.add(addr)
}

func (node *node) RemoveFromConnectingList(addr string) {
	node.ConnectingNodes.del(addr)
}

func (node *node) UpdateInfo(t time.Time, version uint32, services uint64,
	port uint16, nonce uint64, relay uint8, height uint64) {

	node.lastActive = t
	node.id = nonce
	node.version = version
	node.services = services
	node.port = port
	if relay == 0 {
		node.relay = false
	} else {
		node.relay = true
	}
	node.height = uint64(height)
}

func (node *node) NodeDisconnect(v interface{}) {
	if n, ok := node.DelNeighborNode(v.(uint64)); ok {
		log.Debugf("Node [0x%x] disconnected", n.ID())
		n.SetState(p2p.INACTIVITY)
		n.GetConn().Close()
	}
}

func rmNode(node *node) {
	log.Debug(fmt.Sprintf("Remove unused/deuplicate node: 0x%0x", node.id))
}

func (node *node) ID() uint64 {
	return node.id
}

func (node *node) GetConn() net.Conn {
	return node.conn
}

func (node *node) Port() uint16 {
	return node.port
}

func (node *node) IsExternal() bool {
	return node.external
}

func (node *node) HttpInfoPort() int {
	return int(node.httpInfoPort)
}

func (node *node) SetHttpInfoPort(nodeInfoPort uint16) {
	node.httpInfoPort = nodeInfoPort
}

func (node *node) IsRelay() bool {
	return node.relay
}

func (node *node) Version() uint32 {
	return node.version
}

func (node *node) Services() uint64 {
	return node.services
}

func (node *node) IncRxTxnCnt() {
	node.rxTxnCnt++
}

func (node *node) GetTxnCnt() uint64 {
	return node.txnCnt
}

func (node *node) GetRxTxnCnt() uint64 {
	return node.rxTxnCnt
}

func (node *node) Height() uint64 {
	return node.height
}

func (node *node) SetHeight(height uint64) {
	node.height = height
}

func (node *node) Addr() string {
	return node.addr
}

func (node *node) Addr16() ([16]byte, error) {
	var result [16]byte
	ip := net.ParseIP(node.addr).To16()
	if ip == nil {
		log.Error("Parse IP address error\n")
		return result, errors.New("Parse IP address error")
	}

	copy(result[:], ip[:16])
	return result, nil
}

func (node *node) GetTime() int64 {
	t := time.Now()
	return t.UnixNano()
}

func (node *node) Events() *events.Event {
	return node.events
}

func (node *node) WaitForSyncFinish() {
	if len(Parameters.SeedList) <= 0 {
		return
	}
	for {
		log.Trace("BlockHeight is ", chain.DefaultLedger.Blockchain.BlockHeight)
		bc := chain.DefaultLedger.Blockchain
		log.Info("[", len(bc.Index), len(bc.BlockCache), len(bc.Orphans), "]")

		heights := node.GetNeighborHeights()
		log.Trace("others height is ", heights)

		if CompareHeight(uint64(chain.DefaultLedger.Blockchain.BlockHeight), heights) > 0 {
			LocalNode.SetSyncHeaders(false)
			break
		}
		time.Sleep(5 * time.Second)
	}
}

func (node *node) waitForNeighbourConnections() {
	if len(Parameters.SeedList) <= 0 {
		return
	}
	ticker := time.NewTicker(time.Millisecond * 100)
	timer := time.NewTimer(time.Second * 10)
	for {
		select {
		case <-ticker.C:
			if node.GetNeighbourCount() > 0 {
				log.Info("successfully connect to neighbours, neighbour count:", node.GetNeighbourCount())
				return
			}
		case <-timer.C:
			log.Warn("cannot connect to any neighbours, waiting for neighbour connections time out")
			return
		}
	}
}

func (node *node) LoadFilter(filter *msg.FilterLoad) {
	node.filter.Reload(filter)
}

func (node *node) BloomFilter() *bloom.Filter {
	return node.filter
}

func (node *node) Relay(from protocol.Noder, message interface{}) error {
	log.Debug()
	if from != nil && LocalNode.IsSyncHeaders() {
		return nil
	}

	for _, nbr := range node.GetNeighborNodes() {
		if from == nil || nbr.ID() != from.ID() {

			switch message := message.(type) {
			case *Transaction:
				log.Debug("Relay transaction message")
				if nbr.BloomFilter().IsLoaded() && nbr.BloomFilter().MatchTxAndUpdate(message) {
					inv := msg.NewInventory()
					txId := message.Hash()
					inv.AddInvVect(msg.NewInvVect(msg.InvTypeTx, &txId))
					nbr.Send(inv)
					continue
				}

				if nbr.IsRelay() {
					nbr.Send(msg.NewTx(message))
					node.txnCnt++
				}
			case *Block:
				log.Debug("Relay block message")
				if nbr.BloomFilter().IsLoaded() {
					inv := msg.NewInventory()
					blockHash := message.Hash()
					inv.AddInvVect(msg.NewInvVect(msg.InvTypeBlock, &blockHash))
					nbr.Send(inv)
					continue
				}

				if nbr.IsRelay() {
					nbr.Send(msg.NewBlock(message))
				}
			default:
				log.Warn("unknown relay message type")
				return errors.New("unknown relay message type")
			}
		}
	}

	return nil
}

func (node node) IsSyncHeaders() bool {
	node.flagLock.RLock()
	defer node.flagLock.RUnlock()
	if (node.syncFlag & 0x01) == 0x01 {
		return true
	} else {
		return false
	}
}

func (node *node) SetSyncHeaders(b bool) {
	node.flagLock.Lock()
	defer node.flagLock.Unlock()
	if b == true {
		node.syncFlag = node.syncFlag | 0x01
	} else {
		node.syncFlag = node.syncFlag & 0xFE
	}
}

func (node *node) needSync() bool {
	heights := node.GetNeighborHeights()
	log.Info("nbr height-->", heights, chain.DefaultLedger.Blockchain.BlockHeight)
	return CompareHeight(uint64(chain.DefaultLedger.Blockchain.BlockHeight), heights) < 0
}

func CompareHeight(localHeight uint64, heights []uint64) int {
	for _, height := range heights {
		if localHeight < height {
			return -1
		}
	}
	return 1
}

func (node *node) GetRequestBlockList() map[Uint256]time.Time {
	return node.RequestedBlockList
}

func (node *node) IsRequestedBlock(hash Uint256) bool {
	node.requestedBlockLock.Lock()
	defer node.requestedBlockLock.Unlock()
	_, ok := node.RequestedBlockList[hash]
	return ok
}

func (node *node) AddRequestedBlock(hash Uint256) {
	node.requestedBlockLock.Lock()
	defer node.requestedBlockLock.Unlock()
	node.RequestedBlockList[hash] = time.Now()
}

func (node *node) ResetRequestedBlock() {
	node.requestedBlockLock.Lock()
	defer node.requestedBlockLock.Unlock()

	node.RequestedBlockList = make(map[Uint256]time.Time)
}

func (node *node) DeleteRequestedBlock(hash Uint256) {
	node.requestedBlockLock.Lock()
	defer node.requestedBlockLock.Unlock()
	_, ok := node.RequestedBlockList[hash]
	if ok == false {
		return
	}
	delete(node.RequestedBlockList, hash)
}

func (node *node) AcqSyncBlkReqSem() {
	node.SyncBlkReqSem.acquire()
}

func (node *node) RelSyncBlkReqSem() {
	node.SyncBlkReqSem.release()
}

func (node *node) SetStartHash(hash Uint256) {
	node.StartHash = hash
}

func (node *node) GetStartHash() Uint256 {
	return node.StartHash
}

func (node *node) SetStopHash(hash Uint256) {
	node.StopHash = hash
}

func (node *node) GetStopHash() Uint256 {
	return node.StopHash
}
