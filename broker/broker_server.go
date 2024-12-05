package broker

// when doing http stuff
// use rm.Submit(document, crdt) to add entry

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"sync"
)

// type LogEntry struct {
// 	Index int
// 	Edit  any
// 	Term  int
// }

type ServerState int

const (
	Follower  ServerState = 0
	Candidate ServerState = 1
	Leader    ServerState = 2
	Dead      ServerState = 3
)

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

type BrokerServer struct {
	mu sync.Mutex

	// lock for election and replication
	mu2 sync.Mutex

	brokerid int

	// initialize election and replication modules
	em *ElectionModule
	rm *ReplicationModule

	peerIds     []int
	peerClients map[int]*rpc.Client

	listener net.Listener

	// states unique to each server
	state ServerState

	commitChan chan<- CommitEntry

	// rpc proxy used in github to simulate failures
	//rpcProxy *RPCProxy

	// rpc server for handling actual requests
	rpcServer *rpc.Server

	// channel to ensure servers start together
	ready <-chan any
	quit  chan any
	wg    sync.WaitGroup

	// for http
	httpServer *http.Server
	httpAddr   string
	peerAddrs  map[int]string
}

// i think we can just hardcode initialize one server as leader when we start up the cluster?
// ready <-chan any is for make sure everything starts are the same time when close(ready) in whatever starting the servers
func NewBrokerServer(brokerid int, peerIds []int, peerAddrs map[int]string, httpAddr string, state ServerState, ready <-chan any, commitChan chan<- CommitEntry) *BrokerServer {
	broker := new(BrokerServer)
	broker.brokerid = brokerid
	broker.peerIds = peerIds
	broker.peerClients = make(map[int]*rpc.Client)
	broker.state = state
	broker.ready = ready
	broker.commitChan = commitChan
	broker.quit = make(chan any)
	broker.peerAddrs = peerAddrs
	broker.httpAddr = httpAddr

	return broker
}

type CRDTMessage struct { // Type, Index, Value combine to create crdt operation
	Type      string      `json:"type"`  // the crdt operation type {insert, delete}
	Index     int64       `json:"index"` // index of the operation
	Value     interface{} `json:"value"` // chars being inserted / deleted
	ReplicaID string      `json:"replica_id"`
	OpIndex   int64       `json:"operation_index"` // identifies the document the crdt operations edit
	Source    string      `json:"source"`          // "client" or "broker"
}

// http receive to recieve crdts
func (broker *BrokerServer) handleCRTDOperation(w http.ResponseWriter, r *http.Request) {

	if r.Method != http.MethodPost {
		http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	// check first is this broker is leader
	// since our implementation of the appserver multicasts to all nodes
	// when follower recieves message, just ignore
	if broker.state != Leader {
		log.Printf("%s %d ignores CRDT message: Not the leader", broker.state, broker.brokerid) // Redirect to the leader's address
		http.Error(w, "This server is not the leader", http.StatusForbidden)
		return
	}

	var crdtMessage CRDTMessage
	err := json.NewDecoder(r.Body).Decode(&crdtMessage)
	if err != nil {
		http.Error(w, "Invalid CRDT operation payload", http.StatusBadRequest)
		return
	}

	log.Printf("[%d] Received CRDT Message: %+v", broker.brokerid, crdtMessage)

	broker.mu2.Lock()
	defer broker.mu.Unlock()

	// leader builds crdt operation log and submits to ReplicationModule for log replication and committing
	crdtOp := fmt.Sprintf("Type[%s] Index[%d] Value[%+v]", crdtMessage.Type, crdtMessage.Index, crdtMessage.Value)
	documentName := fmt.Sprintf("%d", crdtMessage.OpIndex)

	// submit CRDT Operation to RM
	broker.rm.Submit(documentName, crdtOp)

	log.Printf("%s %d Submits entry %s for document %s", broker.state, broker.brokerid, crdtOp, documentName)

	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte("CRDT operation accepted"))
}

func (broker *BrokerServer) Serve() {

	broker.mu.Lock()

	// initialize election and replication modules for broker server
	broker.em = NewEM(broker.brokerid, broker.peerIds, broker.peerAddrs, broker, broker.ready)
	broker.rm = NewRM(broker.brokerid, broker.peerIds, broker, broker.commitChan)

	// create new rpcServer and register with EM and RM
	broker.rpcServer = rpc.NewServer()
	broker.rpcServer.RegisterName("ElectionModule", broker.em)
	broker.rpcServer.RegisterName("ReplicationModule", broker.rm)

	var err error
	broker.listener, err = net.Listen("tcp", ":0") // listen on any open port
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("[%v] listening at %s", broker.brokerid, broker.listener.Addr())

	broker.mu.Unlock()

	// initialize and start http server to receive crdts from application server
	mux := http.NewServeMux()
	mux.HandleFunc("/crdt", broker.handleCRTDOperation)

	broker.httpServer = &http.Server{
		Addr:    broker.httpAddr,
		Handler: mux,
	}

	log.Printf("[%d] HTTP server listening on %s", broker.brokerid, broker.httpAddr)

	broker.wg.Add(1)

	go func() {
		defer broker.wg.Done()
		if err := broker.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[%d] HTTP server error: %v", broker.brokerid, err)
		}
	}()

	// start listening for requests from other brokers
	broker.wg.Add(1)
	go func() {
		defer broker.wg.Done()
		for {
			conn, err := broker.listener.Accept()
			if err != nil {

				select {
				case <-broker.quit:
					return
				default:
					log.Fatal("accept error:", err)
				}

			}
			broker.wg.Add(1)
			// go routine so that rpc is non blocking
			go func() {
				broker.rpcServer.ServeConn(conn)
				//log.Printf("serve conn")
				broker.wg.Done()
			}()

		}
	}()

	// if follower gets a log update. reject? then app server should resend to leader
}

func (broker *BrokerServer) Call(id int, serviceMethod string, args any, reply any) error {
	broker.mu.Lock()
	peer := broker.peerClients[id]
	broker.mu.Unlock()

	if peer == nil {
		return fmt.Errorf("call client %d after it's closed", id)
	} else {
		//log.Printf("%d makes call to %d", broker.brokerid, id)
		return peer.Call(serviceMethod, args, reply)
	}

	// if err := peer.Call(serviceMethod, args, reply); err != nil {
	// 	// Optionally, try reconnecting or handle error more gracefully
	// 	return fmt.Errorf("failed to call peer %d: %v", id, err)
	// }
	// return nil
}

////////////////////////////////////////////////////////////////////
//THESE FUNCS ARE FOR TESTING AND DEPLOYMENT
////////////////////////////////////////////////////////////////////

// func to connect broker server to a peer when initializing network
func (broker *BrokerServer) ConnectToPeer(peerId int, addr net.Addr) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.peerClients[peerId] == nil {
		client, err := rpc.Dial(addr.Network(), addr.String())
		if err != nil {
			return err
		}
		broker.peerClients[peerId] = client
	}
	return nil
}

// disconnect a server from network
func (broker *BrokerServer) DisconnectPeer(peerId int) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.peerClients[peerId] != nil {
		err := broker.peerClients[peerId].Close()
		broker.peerClients[peerId] = nil
		return err
	}
	return nil
}

func (broker *BrokerServer) DisconnectAll() {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for id := range broker.peerClients {
		if broker.peerClients[id] != nil {
			broker.peerClients[id].Close()
			broker.peerClients[id] = nil
		}
	}
}

// shuts down server
func (broker *BrokerServer) Shutdown() {

	// stop em and rm
	broker.mu2.Lock()
	defer broker.mu2.Unlock()
	broker.state = Dead
	close(broker.rm.newCommitReadyChan)
	close(broker.quit)
	broker.listener.Close()

	// stop http server
	if broker.httpServer != nil {
		if err := broker.httpServer.Close(); err != nil {
			log.Printf("[%d] Error shutting down HTTP server: %v", broker.brokerid, err)
		}
	}

	broker.wg.Wait()
}

func (broker *BrokerServer) GetListenAddr() net.Addr {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.listener.Addr()
}
