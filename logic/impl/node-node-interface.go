package impl

import (
	"fmt"
	"net"
	"net/rpc"
	"log"
	"os"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"time"
	"encoding/gob"
	"encoding/hex"
	"strconv"
	"github.com/rzlim08/GoVector/govec"
	"math/big"
	key "../../key-helpers"
	"../../wolferrors"
	"../../geometry"
	"../../shared"
)

// Node communication interface for communication with other player/logic nodes
type NodeCommInterface struct {
	PlayerNode			*PlayerNode
	PubKey 				*ecdsa.PublicKey
	PrivKey 			*ecdsa.PrivateKey
	Config 				shared.GameConfig
	ServerAddr			string
	ServerConn 			*rpc.Client
	IncomingMessages 	*net.UDPConn
	LocalAddr			net.Addr
	OtherNodes 			map[string]*net.UDPConn
	Log 				*govec.GoLog
	HeartAttack 		chan bool
	MoveCommits			map[string]string
	MessagesToSend		chan *PendingMessage
	NodesToDelete		chan string // Nodes pending delete go here
	NodesToAdd			chan *OtherNode // Nodes pending addition go here
	ACKSReceived		map[uint64] []string // Key: message seq number, Value: nodes that ack-ed
	MovesToSend			chan *PendingMoveUpdates
}

type PendingMessage struct {
	Recipient string
	Message []byte
}

type PendingMoveUpdates struct {
	Seq	uint64
	Coord *shared.Coord
	Rejected int
}

type OtherNode struct {
	Identifier string
	Conn *net.UDPConn
}

type PlayerInfo struct {
	Address 			net.Addr
	PubKey 				ecdsa.PublicKey
}

// The message struct that is sent for all node communcation
type NodeMessage struct {
	Identifier  string             // the id of the sending node
	MessageType string             // identifies the type of message, can be: "move", "moveCommit", "gameState", "connect", "connected", "ack"
	GameState   *shared.GameState  // a gamestate, included if MessageType is "gameState", else nil
	Move        *shared.Coord      // a move, included if the message type is move
	Seq			uint64			   // keep track of seq num for responding ACKs
	MoveCommit  *shared.MoveCommit // a move commit, included if the message type is moveCommit
	Addr        string             // the address to connect to this node over
}

var sequenceNumber uint64 = 0

const REJECTION_MAX = 3

// Creates a node comm interface with initial empty arrays
func CreateNodeCommInterface(pubKey *ecdsa.PublicKey, privKey *ecdsa.PrivateKey, serverAddr string) (NodeCommInterface) {
	return NodeCommInterface {
		PubKey: pubKey,
		PrivKey: privKey,
		ServerAddr : serverAddr,
		OtherNodes: make(map[string]*net.UDPConn),
		HeartAttack: make(chan bool),
		MoveCommits: make(map[string]string),
		MessagesToSend: make(chan *PendingMessage, 30),
		NodesToDelete: make(chan string, 5),
		NodesToAdd: make(chan *OtherNode, 10),
		ACKSReceived: make(map[uint64][]string),
		MovesToSend: make(chan *PendingMoveUpdates),
		}
}

// Runs listener for messages from other nodes, should be run in a goroutine
func (n *NodeCommInterface) RunListener(listener *net.UDPConn, nodeListenerAddr string) {
	// Start the listener
	listener.SetReadBuffer(1048576)

	i := 0
	for {
		i++
		buf := make([]byte, 2048)
		rlen, addr, err := listener.ReadFromUDP(buf)
		if err != nil {
			fmt.Println(err)
		}
		fmt.Println(string(buf[0:rlen]))
		fmt.Println(addr)
		fmt.Println(i)

		message := receiveMessage(n.Log, buf)

		switch message.MessageType {
			case "gameState":
				n.HandleReceivedGameState(message.Identifier, message.GameState)
			case "moveCommit":
				n.HandleReceivedMoveCommit(message.Identifier, message.MoveCommit)
			case "move":
				// Currently only planning to do the lockstep protocol with prey node
				// In the future, may include players close to prey node
				// I.e. check move commits
				if message.Identifier == "prey" {
					n.HandleReceivedMoveL(message.Identifier, message.Move)
				} else {
					n.HandleReceivedMoveNL(message.Identifier, message.Move, message.Seq)
				}
			case "connect":
				n.HandleIncomingConnectionRequest(message.Identifier, message.Addr)
			case "connected":
			// Do nothing
			case "ack":
				n.HandleReceivedAck(message.Addr, message.Seq)
			default:
				fmt.Println("Message type is incorrect")
		}
	}
}

// Routine that handles all reads and writes of the OtherNodes map; single thread preventing concurrent modification
// exception
func (n *NodeCommInterface) ManageOtherNodes() {
	for {
		select {
		case toSend := <-n.MessagesToSend :
			fmt.Println("have message to send")
			if toSend.Recipient != "all" {
				// Send to the single node
				if _, ok := n.OtherNodes[toSend.Recipient]; ok {
					n.OtherNodes[toSend.Recipient].Write(toSend.Message)
				}
			} else {
				// Send the message to all nodes
				n.sendMessageToNodes(toSend.Message)
			}
		case toAdd := <- n.NodesToAdd:
			fmt.Println("have node to add")
			n.OtherNodes[toAdd.Identifier] = toAdd.Conn
		case toDelete := <-n.NodesToDelete:
			fmt.Println("have node to delete")
			delete(n.OtherNodes, toDelete)
		}
	}
}

// Routine that handles the ACKs being received in response to a move message from this node
func (n *NodeCommInterface) ManageAcks() {
	for {
		select {
		case moveToSend := <- n.MovesToSend:
			if values, ok := n.ACKSReceived[moveToSend.Seq]; ok {
				// if the # of acks > # of connected nodes (majority consensus)
				if len(values) > len(n.OtherNodes) {
					n.PlayerNode.GameState.PlayerLocs[n.PlayerNode.Identifier] = *moveToSend.Coord
					// sleep to see if we receive any other acks associated with this seq
					time.Sleep(5 * time.Second)
					// convert array associated with seq to a map
					addresses := make(map[string]string)
					for _, addr := range n.ACKSReceived[moveToSend.Seq] {
						addresses[addr] = ""
					}
					for addr := range n.OtherNodes {
						if _, ok := addresses[addr]; !ok {
							n.NodesToDelete <- addr
						}
					}
					delete(n.ACKSReceived, moveToSend.Seq)
				} else {
					if moveToSend.Rejected < REJECTION_MAX {
						// no majority; so add this back to channel
						moveToSend.Rejected++
						time.Sleep(1 * time.Second)
						n.MovesToSend <- moveToSend
					} else {
						delete(n.ACKSReceived, moveToSend.Seq)
					}
				}
			}
		}
	}
}

func receiveMessage(goLog *govec.GoLog, payload []byte) NodeMessage {
	// Just removes the golog headers from each message
	// TODO: set up error handling
	var message NodeMessage
	goLog.UnpackReceive("LogicNodeReceiveMessage", payload, &message)
	return message
}

func sendMessage(goLog *govec.GoLog, message NodeMessage) []byte{
	newMessage := goLog.PrepareSend("SendMessageToOtherNode", message)
	return newMessage

}
// Registers the node with the server, receiving the gameconfig (and connections)
// TODO: maybe move this into node.go?
func (n *NodeCommInterface) ServerRegister() (id string) {
	gob.Register(&net.UDPAddr{})
	gob.Register(&elliptic.CurveParams{})
	gob.Register(&PlayerInfo{})

	if n.ServerConn == nil {
		response, err := DialAndRegister(n)
		if err != nil {
			os.Exit(1)
		}
		n.Log = govec.InitGoVectorMultipleExecutions("LogicNodeId-"+strconv.Itoa(response.Identifier),
			"LogicNodeFile")

		n.Config = response
	}
	n.GetNodes()

	return strconv.Itoa(n.Config.Identifier)
}
func DialAndRegister(n *NodeCommInterface) (shared.GameConfig, error) {
	// fmt.Printf("DEBUG - ServerRegister() n.ServerConn [%s] should be nil\n", n.ServerConn)
	// Connect to server with RPC, port is always :8081
	serverConn, err := rpc.Dial("tcp", n.ServerAddr)
	if err != nil {
		log.Println("Cannot dial server. Please ensure the server is running and try again.")
		return shared.GameConfig{}, err
	}
	// Storing in object so that we can do other RPC calls outside of this function
	n.ServerConn = serverConn
	var response shared.GameConfig
	// Register with server
	playerInfo := PlayerInfo{n.LocalAddr, *n.PubKey}
	// fmt.Printf("DEBUG - PlayerInfo Struct [%v]\n", playerInfo)
	err = serverConn.Call("GServer.Register", playerInfo, &response)
	if err != nil {
		return shared.GameConfig{}, err
	}
	return response, nil
}

func (n *NodeCommInterface) GetNodes() {
	var response map[string]net.Addr
	err := n.ServerConn.Call("GServer.GetNodes", *n.PubKey, &response)
	if err != nil {
		panic(err)
		log.Fatal(err)
	}

	for id, addr := range response {
		nodeClient := n.GetClientFromAddrString(addr.String())
		nodeUdp, _ := net.ResolveUDPAddr("udp", addr.String())
		// Connect to other node
		nodeClient, err := net.DialUDP("udp", nil, nodeUdp)
		if err != nil {
			panic(err)
		}
		node := OtherNode{Identifier: id, Conn: nodeClient}
		n.NodesToAdd <- &node
		n.InitiateConnection(nodeClient)
	}
}

func (n *NodeCommInterface) GetClientFromAddrString(addr string) (*net.UDPConn) {
	nodeUdp, _ := net.ResolveUDPAddr("udp", addr)
	// Connect to other node
	nodeClient, err := net.DialUDP("udp", nil, nodeUdp)
	if err != nil {
		panic(err)
	}
	return nodeClient
}

func (n *NodeCommInterface) SendHeartbeat() {
	var _ignored bool
	for {
		select {
		case <-n.HeartAttack:
			return
		default:
			err := n.ServerConn.Call("GServer.Heartbeat", *n.PubKey, &_ignored)
			if err != nil {
				fmt.Printf("DEBUG - Heartbeat err: [%s]\n", err)
				n.Config = n.Reregister()

			}
			boop := n.Config.GlobalServerHB
			time.Sleep(time.Duration(boop)*time.Microsecond)
		}
	}
}

func (n* NodeCommInterface) Reregister() shared.GameConfig {
	response, register_failed_err := DialAndRegister(n)
	for register_failed_err != nil {
		response, register_failed_err = DialAndRegister(n)
		time.Sleep(time.Second)
	}
	fmt.Println("Registered Server")
	return response
}

// TODO: Only trying out the sending of ACKS here for now
func(n* NodeCommInterface) SendMoveToNodes(move *shared.Coord){
	if move == nil {
		return
	}

	sequenceNumber++

	message := NodeMessage{
		MessageType: "move",
		Identifier:  n.PlayerNode.Identifier,
		Move:        move,
		Addr:        n.LocalAddr.String(),
		Seq:		 sequenceNumber,
		}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: "all", Message: toSend}
	n.MovesToSend <- &PendingMoveUpdates{Seq: sequenceNumber, Coord: move, Rejected: 0}
}

func (n* NodeCommInterface) SendGameStateToNode(otherNodeId string){
	message := NodeMessage{
		MessageType: "gameState",
		Identifier: n.PlayerNode.Identifier,
		GameState: &n.PlayerNode.GameState,
		Addr: n.LocalAddr.String(),
	}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: otherNodeId, Message: toSend}
}

func (n *NodeCommInterface) SendMoveCommitToNodes(moveCommit *shared.MoveCommit) {
	message := NodeMessage {
		MessageType: "moveCommit",
		Identifier:  n.PlayerNode.Identifier,
		MoveCommit:  moveCommit,
		Addr:        n.LocalAddr.String(),
	}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient:"all", Message: toSend}
}

// Helper function to send message to other nodes
func (n *NodeCommInterface) sendMessageToNodes(toSend []byte) {
	for _, val := range n.OtherNodes{
		_, err := val.Write(toSend)
		if err != nil{
			fmt.Println(err)
		}
	}
}

func (n* NodeCommInterface) HandleReceivedGameState(identifier string, gameState *shared.GameState) {
	//TODO: don't just wholesale replace this
	n.PlayerNode.GameState = *gameState
}

// Handle moves that require a move commit check (lockstep)
func (n* NodeCommInterface) HandleReceivedMoveL(identifier string, move *shared.Coord) (err error) {
	defer delete(n.MoveCommits, identifier)
	// Need nil check for bad move
	if move != nil {
		// if the player has previously submitted a move commit that's the same as the move
		if n.CheckMoveCommitAgainstMove(identifier, *move) {
			// check to see if it's a valid move
			err := n.CheckMoveIsValid(*move)
			if err != nil {
				return err
			}
			n.PlayerNode.GameState.PlayerLocs[identifier] = *move
			return nil
		}
	}
	return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
}

// Handle moves that does not require a move commit check
func (n* NodeCommInterface) HandleReceivedMoveNL(identifier string, move *shared.Coord, seq uint64) (err error) {
	// Need nil check for bad move
	if move != nil {
		err := n.CheckMoveIsValid(*move)
		if err != nil {
			return err
		}
		n.PlayerNode.GameState.PlayerLocs[identifier] = *move
		n.SendACK(identifier, seq)
		return nil
	}
	return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
}

func (n* NodeCommInterface) HandleReceivedMoveCommit(identifier string, moveCommit *shared.MoveCommit) (err error) {
	// if the move is authentic
	if n.CheckAuthenticityOfMoveCommit(moveCommit) {
		// if identifier doesn't exist in map, add move commit to map
		if _, ok := n.MoveCommits[identifier]; !ok {
			n.MoveCommits[identifier] = hex.EncodeToString(moveCommit.MoveHash)
		}
	} else {
		return wolferrors.IncorrectPlayerError(identifier)
	}
	return nil
}

func (n* NodeCommInterface) HandleReceivedAck(addr string, seq uint64) (err error) {
	if _, ok := n.ACKSReceived[seq]; !ok {
		return wolferrors.UnknownSequenceError(seq)
	}
	n.ACKSReceived[seq] = append(n.ACKSReceived[seq], addr)
	return nil
}

func (n* NodeCommInterface) HandleIncomingConnectionRequest(identifier string, addr string) {
	node := n.GetClientFromAddrString(addr)
	n.NodesToAdd <- &OtherNode{Identifier: identifier, Conn: node}
	fmt.Println("adding node to nodetoadd ->", n.NodesToAdd, addr)
}

func (n* NodeCommInterface) InitiateConnection(nodeClient *net.UDPConn) {
	message := NodeMessage{
		MessageType: "connect",
		Identifier:  strconv.Itoa(n.Config.Identifier),
		GameState:   nil,
		Addr:        n.LocalAddr.String(),
		Move:        nil,
	}
	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: "all", Message: toSend}
}

// Sends connection message to connections after receiving from server
func (n *  NodeCommInterface) FloodNodes() {
	for _, node := range n.OtherNodes {
		// Exchange messages with other node
		n.InitiateConnection(node)
	}
}

func (n *NodeCommInterface) SendACK(identifier string, seq uint64) {
	message := NodeMessage {
		MessageType: "ack",
		Identifier: n.PlayerNode.Identifier,
		Seq: seq,
		Addr: n.LocalAddr.String(),
	}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: identifier, Message: toSend}
}

////////////////////////////////////////////// MOVE COMMIT HASH FUNCTIONS //////////////////////////////////////////////

// Calculate the hash of the coordinates which will be sent at the move commitment stage
func CalculateHash(m shared.Coord, id string) ([]byte) {
	hash := md5.New()
	arr := make([]byte, 2048)

	arr = strconv.AppendInt(arr, int64(m.X), 10)
	arr = strconv.AppendInt(arr, int64(m.Y), 10)
	arr = strconv.AppendQuote(arr, id)

	// Write the hash
	hash.Write(arr)
	return hash.Sum(nil)
}

// Sign the move commit with private key
func (n *NodeCommInterface) SignMoveCommit(hash []byte) (r, s *big.Int, err error) {
	return ecdsa.Sign(rand.Reader, n.PrivKey, hash)
}

// Checks to see if the hash is legit
func (n *NodeCommInterface) CheckAuthenticityOfMoveCommit(m *shared.MoveCommit) (bool) {
	publicKey := key.PublicKeyStringToKey(m.PubKey)
	rBigInt := new(big.Int)
	_, err := fmt.Sscan(m.R, rBigInt)

	sBigInt := new(big.Int)
	_, err = fmt.Sscan(m.S, sBigInt)
	if err != nil {
		fmt.Println("Trouble converting string to big int")
	}
	return ecdsa.Verify(publicKey, m.MoveHash, rBigInt, sBigInt)
}

////////////////////////////////////////////// MOVE CHECK FUNCTIONS ////////////////////////////////////////////////////

// Checks to see if there is an existing commit against the submitted move
func (n *NodeCommInterface) CheckMoveCommitAgainstMove(identifier string, move shared.Coord) (bool) {
	hash := hex.EncodeToString(CalculateHash(move, identifier))
	for i, mc := range n.MoveCommits {
		if mc == hash && i == identifier {
			return true
		}
	}
	return false
}

// Check move to see if it's valid based on this node's game state
func (n *NodeCommInterface) CheckMoveIsValid(move shared.Coord) (err error) {
	gridManager := geometry.CreateNewGridManager(n.PlayerNode.GameConfig.Settings)
	if !gridManager.IsInBounds(move) {
		return wolferrors.OutOfBoundsError("[" + string(move.X) + ", " + string(move.Y) + "]")
	}
	if !gridManager.IsValidMove(move) {
		return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
	}
	return nil
}