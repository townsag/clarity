package broker

import (
	"log"
	"net"
	"sync"
)

type LogEntry struct {
	Index int
	Edit  any
	Term  int
}

type BrokerServer struct {
	mu sync.Mutex

	brokerid int

	peerIds []int

	listener net.Listener

	// persistent state on all servers
	// should be replicated across all brokers
	term int // what is the current term
	vote int // who this server voted for
	log  []LogEntry

	// states unique to each server
	state ServerState

	// channel to ensure servers start together
	ready <-chan any
}

type ServerState int

const (
	Follower  ServerState = 0
	Candidate ServerState = 1
	Leader    ServerState = 2
	Dead      ServerState = 3
)

// converts CMState values into strings
// for debugging and terminal prints
func (s ServerState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case Dead:
		return "Dead"
	default:
		panic("unreachable")
	}
}

// i think we can just hardcode initialize one server as leader when we start up the cluster?
// ready <-chan any is for make sure everything starts are the same time when close(ready) in whatever starting the servers
func NewBrokerServer(brokerid int, peerIds []int, state ServerState, ready <-chan any) *BrokerServer {
	broker := new(BrokerServer)
	broker.brokerid = brokerid
	broker.peerIds = peerIds
	broker.state = state
	broker.ready = ready

	return broker
}

// Broker Server's main routine
// each broker must:
//
//	If leader:
//		recieve CRDT operations from application server/s
//		update own log and send update to followers
//		make sure enough followers recieved update then tell all followers to commit
//		heartbeat to followers and application servers
//		handle application server polls and respond with the correct log
//	if follower:
//		handle application server polls and respond with the correct log
//		maintain consistency with leader
//		maintain timeout to elect new leader of leader is dead (no heartbeat)
//
// gossip to detect FOLLOWER node failures
func (broker *BrokerServer) Serve() {

	broker.mu.Lock()

	var err error
	broker.listener, err = net.Listen("tcp", ":0") // listen on any open port
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("[%v] listening at %s", broker.brokerid, broker.listener.Addr())

	broker.mu.Unlock()

	// if follower gets a log update. reject? then app server should resend to leader
}

// func main() {

// 	listener, err := net.Listen("tcp", "localhost:8000")
// 	if err != nil {
// 		fmt.Println("Error:", err)
// 		return
// 	}
// 	defer listener.Close()

// 	fmt.Println("Server is listening on port 8080")

// 	for {
// 		// Accept incoming connections
// 		conn, err := listener.Accept()
// 		if err != nil {
// 			fmt.Println("Error:", err)
// 			continue
// 		}

// 		// Handle client connection in a goroutine
// 		go handleClient(conn)
// 	}

// }

// func handleClient(conn net.Conn) {
// 	defer conn.Close()

// 	// Read and process data from the client
// 	// ...

// 	// Write data back to the client
// 	// ...
// }
