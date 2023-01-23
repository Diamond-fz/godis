package cluster

import (
	"fmt"
	"github.com/hdt3213/godis/config"
	"github.com/hdt3213/godis/database"
	"github.com/hdt3213/godis/interface/redis"
	"github.com/hdt3213/godis/lib/logger"
	"github.com/hdt3213/godis/lib/utils"
	"github.com/hdt3213/godis/redis/client"
	"github.com/hdt3213/godis/redis/connection"
	"github.com/hdt3213/godis/redis/parser"
	"github.com/hdt3213/godis/redis/protocol"
	"hash/crc32"
	"net"
	"sort"
	"strconv"
	"time"
)

// Node represents a node and its slots, used in cluster internal messages
type Node struct {
	ID        string
	Addr      string
	Slots     []*Slot // ascending order by slot id
	Flags     uint32
	lastHeard time.Time
}

const (
	nodeFlagLeader uint32 = 1 << iota
	nodeFlagCandidate
	nodeFlagLearner
)

const (
	follower raftState = iota
	leader
	candidate
	learner
)

func (node *Node) setState(state raftState) {
	node.Flags &= ^uint32(0x7) // clean
	switch state {
	case follower:
		break
	case leader:
		node.Flags |= nodeFlagLeader
	case candidate:
		node.Flags |= nodeFlagCandidate
	case learner:
		node.Flags |= nodeFlagLearner
	}
}

func (node *Node) getState() raftState {
	if node.Flags&nodeFlagLeader > 0 {
		return leader
	}
	if node.Flags&nodeFlagCandidate > 0 {
		return candidate
	}
	if node.Flags&nodeFlagLearner > 0 {
		return learner
	}
	return follower
}

const (
	slotFlagMigrating uint32 = 1 << iota
)

// Slot represents a hash slot,  used in cluster internal messages
type Slot struct {
	// ID is uint between 0 and 16383
	ID uint32
	// NodeID is id of the hosting node
	// If the slot is migrating, NodeID is the id of the node importing this slot (target node)
	NodeID string
	// OldNodeID is the node which is moving out this slot
	// only valid during slot is migrating
	OldNodeID string
	// Flags stores more information of slot
	Flags uint32
}

func (slot *Slot) IsMigrating() bool {
	return slot.Flags&slotFlagMigrating > 0
}

func getSlot(key string) uint32 {
	return crc32.ChecksumIEEE([]byte(key)) % uint32(slotCount)
}

func (cluster *Cluster) startAsSeed() error {
	selfNodeId, err := cluster.topology.StartAsSeed(config.Properties.AnnounceAddress())
	if err != nil {
		return err
	}
	for i := 0; i < slotCount; i++ {
		cluster.setSlot(uint32(i), slotStateHost)
	}
	cluster.self = selfNodeId
	return nil
}

// findSlotsForNewNode try to find slots for new node, but do not actually migrate
func (cluster *Cluster) findSlotsForNewNode() []*Slot {
	nodeMap := cluster.topology.GetTopology() // including the new node
	avgSlot := slotCount / len(nodeMap)
	nodes := make([]*Node, 0, len(nodeMap))
	for _, node := range nodeMap {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return len(nodes[i].Slots) > len(nodes[j].Slots)
	})
	result := make([]*Slot, 0, avgSlot)
	// there are always some nodes has more slots than avgSlot
	for _, node := range nodes {
		if len(node.Slots) <= avgSlot {
			// nodes are in decreasing order by len(node.Slots)
			// if len(node.Slots) < avgSlot, then all following nodes has fewer slots than avgSlot
			break
		}
		n := 2*avgSlot - len(result)
		if n < len(node.Slots) {
			// n - len(result) - avgSlot = avgSlot - len(result)
			// now len(result) == avgSlot
			result = append(result, node.Slots[avgSlot:n]...)
		} else {
			result = append(result, node.Slots[avgSlot:]...)
		}
		if len(result) >= avgSlot {
			break
		}
	}
	return result
}

// Join send `gcluster join` to node in cluster to join
func (cluster *Cluster) Join(seed string) protocol.ErrorReply {
	seedCli, err := client.MakeClient(seed)
	if err != nil {
		return protocol.MakeErrReply("connect with seed failed: " + err.Error())
	}
	seedCli.Start()
	// todo: auth
	ret := seedCli.Send(utils.ToCmdLine("raft", "get-leader"))
	if protocol.IsErrorReply(ret) {
		return ret.(protocol.ErrorReply)
	}
	leaderInfo, ok := ret.(*protocol.MultiBulkReply)
	if !ok || len(leaderInfo.Args) != 2 {
		return protocol.MakeErrReply("ERR get-leader returns wrong reply")
	}
	leaderAddr := string(leaderInfo.Args[1])
	leaderCli, err := client.MakeClient(leaderAddr)
	// todo: auth
	if err != nil {
		return protocol.MakeErrReply("connect with seed failed: " + err.Error())
	}
	leaderCli.Start()
	ret = leaderCli.Send(utils.ToCmdLine("raft", "join", config.Properties.AnnounceAddress()))
	// todo: handle NOT LEADER error
	if protocol.IsErrorReply(ret) {
		return ret.(protocol.ErrorReply)
	}
	topology, ok := ret.(*protocol.MultiBulkReply)
	if !ok || len(topology.Args) < 4 {
		return protocol.MakeErrReply("ERR gcluster join returns wrong reply")
	}
	selfNodeId := string(topology.Args[0])
	leaderId := string(topology.Args[1])
	term, _ := strconv.Atoi(string(topology.Args[2]))
	commitIndex, _ := strconv.Atoi(string(topology.Args[3]))
	nodes, err := unmarshalTopology(topology.Args[4:])
	if err != nil {
		return protocol.MakeErrReply(err.Error())
	}
	cluster.topology.Load(selfNodeId, leaderId, term, commitIndex, nodes)
	cluster.self = selfNodeId
	cluster.topology.start(follower)
	// asynchronous migrating slots
	go func() {
		time.Sleep(time.Second) // let the cluster started
		cluster.rebalance(err)
	}()
	return nil
}

func (cluster *Cluster) rebalance(err error) {
	slots := cluster.findSlotsForNewNode()
	// serial migrations to avoid overloading the cluster
	for _, slot := range slots {
		if slot.IsMigrating() {
			continue
		}
		logger.Info("start import slot ", slot.ID)
		err = cluster.importSlot(slot)
		if err != nil {
			logger.Error(fmt.Sprintf("import slot %d error: %d", slot.ID, err))
			// todo: delete all keys in slot
			continue
		}
		logger.Info("finish import slot", slot.ID)
	}
}

func (cluster *Cluster) importSlot(slot *Slot) error {
	fakeConn := connection.NewFakeConn()
	node := cluster.topology.PickNode(slot.ID)
	conn, err := net.Dial("tcp", node.Addr)
	if err != nil {
		return fmt.Errorf("connect with %s(%s) error: %v", node.ID, node.Addr, err)
	}
	nodeChan := parser.ParseStream(conn)
	send2node := func(cmdLine CmdLine) redis.Reply {
		req := protocol.MakeMultiBulkReply(cmdLine)
		_, err := conn.Write(req.ToBytes())
		if err != nil {
			return protocol.MakeErrReply(err.Error())
		}
		resp := <-nodeChan
		if resp.Err != nil {
			return protocol.MakeErrReply(resp.Err.Error())
		}
		return resp.Data
	}

	cluster.setSlot(slot.ID, slotStateImporting) // prepare host slot before send `set slot`
	cluster.topology.setLocalSlotMigrating(slot.ID, cluster.self)
	ret := send2node(utils.ToCmdLine(
		"gcluster", "set-slot", strconv.Itoa(int(slot.ID)), cluster.self))
	if !protocol.IsOKReply(ret) {
		return fmt.Errorf("set slot %d error: %v", slot.ID, err)
	}
	logger.Info(fmt.Sprintf("set slot %d to current node, start migrate", slot.ID))

	req := protocol.MakeMultiBulkReply(utils.ToCmdLine(
		"gcluster", "migrate", strconv.Itoa(int(slot.ID)), cluster.self))
	_, err = conn.Write(req.ToBytes())
	if err != nil {
		return protocol.MakeErrReply(err.Error())
	}
slotLoop:
	for proto := range nodeChan {
		if proto.Err != nil {
			return fmt.Errorf("set slot %d error: %v", slot.ID, err)
		}
		switch reply := proto.Data.(type) {
		case *protocol.MultiBulkReply:
			// todo: handle exec error
			_ = cluster.db.Exec(fakeConn, reply.Args)
			keys, _ := database.GetRelatedKeys(reply.Args)
			for _, key := range keys {
				cluster.setImportedKey(key)
			}
		case *protocol.StatusReply:
			if protocol.IsOKReply(reply) {
				break slotLoop
			}
		}
	}
	cluster.slots[slot.ID].importedKeys = nil
	cluster.slots[slot.ID].state = slotStateHost
	cluster.FinishSlotMigrate(slot.ID)
	send2node(utils.ToCmdLine("gcluster", "migrate-done", strconv.Itoa(int(slot.ID))))
	return nil
}

func (cluster *Cluster) FinishSlotMigrate(slotID uint32) {
	// todo: raft 不再关注迁移状态信息, 只关心由谁负责 slot
	raft := cluster.topology
	slot := raft.slots[int(slotID)]
	oldNode := raft.nodes[slot.OldNodeID]
	// remove from old oldNode
	for i, s := range oldNode.Slots {
		if s.ID == slot.ID {
			copy(oldNode.Slots[i:], oldNode.Slots[i+1:])
			oldNode.Slots = oldNode.Slots[:len(oldNode.Slots)-1]
			break
		}
	}
	// add into new node
	newNode := raft.nodes[slot.NodeID]
	newNode.Slots = append(newNode.Slots, slot)
	slot.Flags &= ^slotFlagMigrating
	slot.OldNodeID = ""
}