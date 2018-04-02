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

// Node communication interface for communication with other player/logic nodes as well as the server
type NodeCommInterface struct {
	// A reference back to this interface's "main" node
	PlayerNode			*PlayerNode

	// The public key of this nodes
	PubKey 				*ecdsa.PublicKey

	// The private key of this node, used to encrypt messages
	PrivKey 			*ecdsa.PrivateKey

	// The gameconfig for the game, primarily used here to form connections to the given nodes
	Config 				shared.GameConfig

	// The address of the server for this game
	ServerAddr			string

	// The RPC connection to the server
	ServerConn 			*rpc.Client

	// The UDP connection over which this node listens for messages from other logic nodes
	IncomingMessages 	*net.UDPConn

	// The address of this node's listener
	LocalAddr			net.Addr

	// The current map of identifiers to connections of nodes in play
	OtherNodes 			map[string]*net.UDPConn

	// The GoVector log
	Log 				*govec.GoLog

	// A channel that, when written to, will stop heartbeats. Primarily for testing
	HeartAttack 		chan bool

	// A map to store move commits in before receiving their associated moves
	MoveCommits			map[string]string

	// Channel that messages are written to so they can be handled by the goroutine that deals with sending messages
	// and managing the player nodes
	MessagesToSend		chan *PendingMessage

	// Channel that the identifiers of nodes to delete are added to so they can be handled by the goroutine that deals
	// with sending messages and managing the player nodes
	NodesToDelete		chan string

	// Channel that the identifiers and connections of nodes to add to other nodes are sent to so they can be handled
	// by the goroutine that deals with sending messages and managing the player nodes
	NodesToAdd			chan *OtherNode
}

// A message for another node with a recipient and a byte-encoded message. If the recipient is "all", the message is
// sent to every node in OtherNodes.
type PendingMessage struct {
	Recipient string
	Message []byte
}

// An othernode struct, used for storing node ids/conns before they are added to the OtherNodes map
type OtherNode struct {
	Identifier string
	Conn *net.UDPConn
}

// A playerinfo struct, provides identification information about this node: the address and public key
type PlayerInfo struct {
	Address 			net.Addr
	PubKey 				ecdsa.PublicKey
}

// The message struct that is sent for all node communication
type NodeMessage struct {

	// the id of the sending node
	Identifier  string

	// identifies the type of message
	// can be: "move", "moveCommit", "gameState", "connect", "connected"
	MessageType string

	// a gamestate, included if MessageType is "gameState", else nil
	GameState   *shared.GameState

	// a move, included if the message type is move
	Move        *shared.Coord

	// a move commit, included if the message type is moveCommit
	MoveCommit  *shared.MoveCommit

	// a score, included if the message is a preyCapture
	Score int

	// the address to connect to the sending node over
	Addr        string
}

// Creates a node comm interface with initial empty arrays/maps
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
		}
}

// Runs listener for messages from other nodes, should be run in a goroutine
// Unmarshalls received messages and dispatches them to the appropriate handler function
func (n *NodeCommInterface) RunListener(listener *net.UDPConn, nodeListenerAddr string) {
	// Start the listener
	listener.SetReadBuffer(1048576)

	i := 0
	for {
		i++
		buf := make([]byte, 2048)
		_, _, err := listener.ReadFromUDP(buf)
		if err != nil {
			fmt.Println(err)
		}

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
				n.PlayerNode.pixelInterface.SendPlayerGameState(n.PlayerNode.GameState)
				if message.Identifier == "prey" {
					err := n.HandleReceivedMoveL(message.Identifier, message.Move)
					if err != nil {
						fmt.Println("The error in the prey moving")
						fmt.Println(err)
					}
				} else {
					n.HandleReceivedMoveNL(message.Identifier, message.Move)
				}
			case "connect":
				n.HandleIncomingConnectionRequest(message.Identifier, message.Addr)
			case "connected":
			// Do nothing
			case "captured":
				n.HandleCapturedPreyRequest(message.Identifier, message.Move, message.Score)
			default:
				fmt.Println("Message type is incorrect")
		}
	}
}

// Routine that handles all reads and writes of the OtherNodes map; single thread preventing concurrent iteration and write
// exception. This routine therefore handles all sending of messages as well as that requires iteration over OtherNodes.
func (n *NodeCommInterface) ManageOtherNodes() {
	for {
		select {
		case toSend := <-n.MessagesToSend :
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
			n.OtherNodes[toAdd.Identifier] = toAdd.Conn
		case toDelete := <-n.NodesToDelete:
			delete(n.OtherNodes, toDelete)
		}
	}
}

// Helper function that unpacks the GoVector message tooling
// Returns the unmarshalled NodeMessage, ready for reading
func receiveMessage(goLog *govec.GoLog, payload []byte) NodeMessage {
	// Just removes the golog headers from each message
	// TODO: set up error handling
	var message NodeMessage
	goLog.UnpackReceive("LogicNodeReceiveMessage", payload, &message)
	return message
}

// Helper function that packs the GoVector message tooling
// Returns the byte-encoded message, ready to send
func sendMessage(goLog *govec.GoLog, message NodeMessage) []byte{
	newMessage := goLog.PrepareSend("SendMessageToOtherNode", message)
	return newMessage

}
// Registers the node with the server, receiving the game config (and connections)
// Returns the unique id of this node assigned by the server
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

// Another server registration function, used to deal with server disconnection.
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

// Requests the list of currently connected nodes from the server, and initiates a connection with them
func (n *NodeCommInterface) GetNodes() {
	var response map[string]net.Addr
	err := n.ServerConn.Call("GServer.GetNodes", *n.PubKey, &response)
	if err != nil {
		panic(err)
		log.Fatal(err)
	}

	for id, addr := range response {
		nodeClient := n.GetClientFromAddrString(addr.String())
		node := OtherNode{Identifier: id, Conn: nodeClient}
		n.NodesToAdd <- &node
		n.InitiateConnection(nodeClient)
	}
}

// Takes in an address string and makes a UDP connection to the client specified by the string. Returns the connection.
func (n *NodeCommInterface) GetClientFromAddrString(addr string) (*net.UDPConn) {
	nodeUdp, _ := net.ResolveUDPAddr("udp", addr)
	// Connect to other node
	nodeClient, err := net.DialUDP("udp", nil, nodeUdp)
	if err != nil {
		panic(err)
	}
	return nodeClient
}

// Sends a heartbeat to the server at the interval specificed at server registration
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
				n.Config  = n.Reregister()

			}
			boop := n.Config.GlobalServerHB
			time.Sleep(time.Duration(boop)*time.Microsecond)
		}
	}
}

// Function that is started when the server dies; will continue to reregister until the server comes back up
func (n* NodeCommInterface)Reregister()shared.GameConfig{
	response, register_failed_err := DialAndRegister(n)
	for register_failed_err != nil{
		response, register_failed_err = DialAndRegister(n)
		time.Sleep(time.Second)
	}
	fmt.Println("Registered Server")
	return response
}

// Takes in a new coordinate for this node and sends it to all other nodes.
func(n* NodeCommInterface) SendMoveToNodes(move *shared.Coord){
	if move == nil {
		return
	}

	message := NodeMessage{
		MessageType: "move",
		Identifier:  n.PlayerNode.Identifier,
		Move:        move,
		Addr:        n.LocalAddr.String(),
		}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: "all", Message: toSend}
}

func(n* NodeCommInterface) SendPreyCaptureToNodes(move *shared.Coord, score int) {
	if move == nil {
		return
	}

	message := NodeMessage{
		MessageType: "captured",
		Identifier: n.PlayerNode.Identifier,
		Move:	move,
		Score: score,
		Addr: n.LocalAddr.String(),
	}

	toSend := sendMessage(n.Log, message)
	n.MessagesToSend <- &PendingMessage{Recipient: "all", Message: toSend}
}

// Takes in a node ID and sends this node's gamestate to that node
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

// Sends a move commit to all other nodes, for lockstep protocol
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

// Helper function to send message to other nodes; do not call directly; instead write to the messagesTosend channel
func (n *NodeCommInterface) sendMessageToNodes(toSend []byte) {
	for _, val := range n.OtherNodes{
		_, err := val.Write(toSend)
		if err != nil{
			fmt.Println(err)
		}
	}
}

// Handles a gamestate received from another node.
func (n* NodeCommInterface) HandleReceivedGameState(identifier string, gameState *shared.GameState) {
	//TODO: don't just wholesale replace this
	n.PlayerNode.GameState = *gameState
}

// Handle moves that require a move commit check (lockstep)
// Returns an InvalidMoveError if the move does not match a received commit
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
			n.PlayerNode.GameState.PlayerLocs.Lock()
			n.PlayerNode.GameState.PlayerLocs.Data[identifier] = *move
			n.PlayerNode.GameState.PlayerLocs.Unlock()
			return nil
		}
	}
	return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
}

// Handle moves that does not require a move commit check
// Returns InvalidMoveError if the received move is not valid
func (n* NodeCommInterface) HandleReceivedMoveNL(identifier string, move *shared.Coord) (err error) {
	// Need nil check for bad move
	if move != nil {
		err := n.CheckMoveIsValid(*move)
		if err != nil {
			return err
		}
		n.PlayerNode.GameState.PlayerLocs.Lock()
		n.PlayerNode.GameState.PlayerLocs.Data[identifier] = *move
		n.PlayerNode.GameState.PlayerLocs.Unlock()
		return nil
	}
	return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
}

// Handles received move commits from other nodes by storing them in anticipation of receiving a move
// Returns IncorrectPlayerError if the player that send the message is not the player they are claiming to be
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

// Handles "connect" messages received by other nodes by adding the incoming node to this node's OtherNodes
func (n* NodeCommInterface) HandleIncomingConnectionRequest(identifier string, addr string) {
	node := n.GetClientFromAddrString(addr)
	n.NodesToAdd <- &OtherNode{Identifier: identifier, Conn: node}
}

func (n* NodeCommInterface) HandleCapturedPreyRequest(identifier string, move *shared.Coord, score int) (err error) {
	err = n.CheckGotPrey(*move)
	if err != nil {
		return err
	}
	err = n.CheckMoveIsValid(*move)
	if err != nil {
		return err
	}
	playerScore := n.PlayerNode.GameState.PlayerScores[identifier]
	if playerScore != playerScore + 1 {
		return wolferrors.InvalidScoreUpdateError(string(score))
	}
	playerScore = playerScore + 1
	return nil
}

// Initiates a connection to another node by sending it a "connect" message
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

////////////////////////////////////////////// MOVE COMMIT HASH FUNCTIONS //////////////////////////////////////////////

// Calculate the hash of the coordinates which will be sent at the move commitment stage
func (n *NodeCommInterface) CalculateHash(m shared.Coord, id string) ([]byte) {
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
	hash := hex.EncodeToString(n.CalculateHash(move, identifier))
	for i, mc := range n.MoveCommits {
		if mc == hash && i == identifier {
			return true
		}
	}
	return false
}

// Check move to see if it's valid based on the gameplay grid
func (n *NodeCommInterface) CheckMoveIsValid(move shared.Coord) (err error) {
	gridManager := geometry.CreateNewGridManager(n.PlayerNode.GameConfig.Settings)
	if !gridManager.IsValidMove(move) {
		return wolferrors.InvalidMoveError("[" + string(move.X) + ", " + string(move.Y) + "]")
	}
	return nil
}

func (n *NodeCommInterface) CheckGotPrey(move shared.Coord) (err error) {
	if move.X == n.PlayerNode.GameState.PlayerLocs.Data["prey"].X &&
		move.Y == n.PlayerNode.GameState.PlayerLocs.Data["prey"].Y {
		return nil
	}
	return wolferrors.InvalidPreyCaptureError("[" + string(move.X) + ", " + string(move.Y) + "]")
}