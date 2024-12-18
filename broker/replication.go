package broker

import (
	"log"
)

type CommitEntry struct {
	CRDTOperation any

	Index int

	Term int
}

type LogEntry struct {
	CRDTOperation any
	Term          int
	Document      string
}

type ReplicationModule struct {
	broker *BrokerServer

	// id of connected server
	id int

	peerIds []int

	// working log structure for appends
	log []LogEntry

	// storage for committed log entries
	committedLog []LogEntry

	commitIndex int

	commitChan chan<- CommitEntry

	// channel to coordiate commits
	// added to in leaderSendAEs and AppendEntries
	// consumed in commitChanSender
	newCommitReadyChan chan struct{}

	// AE stands for appendentry. used also for heartbeat
	triggerAEChan chan struct{}

	lastApplied int
}

func NewRM(id int, peerIds []int, broker *BrokerServer, commitChan chan<- CommitEntry) *ReplicationModule {

	rm := new(ReplicationModule)

	rm.broker = broker
	rm.id = id
	rm.peerIds = peerIds
	rm.commitIndex = -1

	rm.commitChan = commitChan

	// channels are like temporary storage that will be consumed by some function

	// 16 is buffer size. it means that 100 notifs can be held in channel;
	rm.newCommitReadyChan = make(chan struct{}, 100)

	// 1 ensures only 1 AppendEntry is pending
	rm.triggerAEChan = make(chan struct{}, 1)

	go rm.commitChanSender()

	return rm
}

// main function for leader to send AppendEntry commands to followers
// also used in election.go for heartbeat
func (rm *ReplicationModule) leaderSendAEs() {
	rm.broker.mu2.Lock()

	// if broker is not leader. don't let it send AppendEntries
	if rm.broker.state != Leader {
		rm.broker.mu2.Unlock()
		return
	}

	currentTerm := rm.broker.em.term
	rm.broker.mu2.Unlock()

	for _, peerId := range rm.peerIds {

		// get the most recent index of the leader's log
		// replication for followers will start from there
		go func(peerId int) {
			rm.broker.mu2.Lock()
			nextIndex := rm.broker.em.nextIndex[peerId]

			prevLogIndex := nextIndex - 1
			prevLogTerm := -1

			if prevLogIndex >= 0 {
				prevLogTerm = rm.log[prevLogIndex].Term
			}
			entries := rm.log[nextIndex:]

			args := AppendEntriesArgs{
				Term:         currentTerm,
				LeaderId:     rm.id,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      entries,
				LeaderCommit: rm.commitIndex,
			}
			rm.broker.mu2.Unlock()

			log.Printf("%d sending AE Call to %d: %+v", rm.id, peerId, args)

			var reply AppendEntriesReply
			if err := rm.broker.Call(peerId, "ReplicationModule.AppendEntries", args, &reply); err == nil {
				log.Printf("%s %d receives AE reply from %d", rm.broker.state, rm.id, reply.Id)
				rm.broker.mu2.Lock()

				// if it detects through heartbeat that own term is out of date, become follower
				if reply.Term > rm.broker.em.term {
					log.Printf("leader %d's term is outdated", rm.id)
					rm.broker.em.becomeFollower(reply.Term)
					rm.broker.mu2.Unlock()
					return
				}

				// if broker is leader and it's term is up to date
				if rm.broker.state == Leader && currentTerm == reply.Term {
					if reply.Success {
						log.Printf("%d replies successful append", reply.Id)
						rm.broker.em.nextIndex[peerId] = nextIndex + len(entries)
						rm.broker.em.matchIndex[peerId] = rm.broker.em.nextIndex[peerId] - 1

						// get replies from followers to decide whether or not to send commit
						savedCommitIndex := rm.commitIndex
						for i := rm.commitIndex + 1; i < len(rm.log); i++ {
							if rm.log[i].Term == rm.broker.em.term {
								matches := 1
								for _, peerId := range rm.peerIds {
									if rm.broker.em.matchIndex[peerId] >= i {
										log.Printf("%d is ready to commit", peerId)
										matches++
									}
								}
								// currently set to atomic. real raft does majority
								// if matches*2 > len(rm.peerIds)+1
								if matches == len(rm.peerIds) {
									log.Printf("all followers ready to commit, %s %d updates commitIndex to %d", rm.broker.state, rm.id, i)

									rm.commitIndex = i
								}
							}

						}
						// notify followers of commit
						if rm.commitIndex != savedCommitIndex {
							rm.broker.mu2.Unlock()
							rm.newCommitReadyChan <- struct{}{}
							rm.triggerAEChan <- struct{}{}
						} else {
							rm.broker.mu2.Unlock()
						}

					} else { // if reply.success = false
						if reply.ConflictTerm >= 0 {
							lastIndexOfTerm := -1
							for i := len(rm.log) - 1; i >= 0; i-- {
								if rm.log[i].Term == reply.ConflictTerm {
									lastIndexOfTerm = i
									break
								}
							}

							if lastIndexOfTerm >= 0 {
								rm.broker.em.nextIndex[peerId] = lastIndexOfTerm + 1
							} else {
								rm.broker.em.nextIndex[peerId] = reply.ConflictIndex
							}
						} else {
							rm.broker.em.nextIndex[peerId] = reply.ConflictIndex
						}

						rm.broker.mu2.Unlock()
					}

				} else {
					rm.broker.mu2.Unlock()
				}

			}

		}(peerId)
	}

}

func (rm *ReplicationModule) commitChanSender() {

	for range rm.newCommitReadyChan {
		log.Printf("enter commitChanSender()")
		rm.broker.mu2.Lock()
		savedTerm := rm.broker.em.term
		savedLastApplied := rm.lastApplied

		var entries []LogEntry
		//log.Printf("in commitChanSender lastApplied: %d   commitIndex: %d", rm.lastApplied, rm.commitIndex)

		// handle base case for first commit
		if rm.commitIndex == 0 {
			entries = rm.log[rm.lastApplied : rm.commitIndex+1]
			rm.lastApplied = rm.commitIndex
		} else if rm.commitIndex > rm.lastApplied { // standard case for subsequent commits
			entries = rm.log[rm.lastApplied+1 : rm.commitIndex+1]
			rm.lastApplied = rm.commitIndex
		}
		rm.broker.mu2.Unlock()
		log.Printf("%s %d commitChanSender entries=%v, savedLastApplied=%d", rm.broker.state, rm.id, entries, savedLastApplied)

		for i, entry := range entries {
			// add committed entry to committedLog
			rm.committedLog = append(rm.committedLog, entry)

			rm.commitChan <- CommitEntry{
				CRDTOperation: entry.CRDTOperation,
				Index:         savedLastApplied + i + 1,
				Term:          savedTerm,
			}
			log.Printf("%s %d committed %+v to committedLog", rm.broker.state, rm.id, entry)
		}
	}
}

// rpc request from leader to follower
// handles both heartbeat and actual log entries
type AppendEntriesArgs struct {
	Term     int
	LeaderId int

	PrevLogIndex int
	PrevLogTerm  int

	Entries []LogEntry

	LeaderCommit int
}

// rpc reply from follower to leader
type AppendEntriesReply struct {
	Term    int
	Success bool
	Id      int

	ConflictIndex int
	ConflictTerm  int
}

// this func is primarily for followers to accept replication from leader
func (rm *ReplicationModule) AppendEntries(args AppendEntriesArgs, reply *AppendEntriesReply) error {
	log.Printf("%s %d received AE from %d: %+v", rm.broker.state, rm.id, args.LeaderId, args)
	rm.broker.mu2.Lock()
	defer rm.broker.mu2.Unlock()

	// if log entry to append has higher term. become follower
	if args.Term > rm.broker.em.term {
		rm.broker.em.becomeFollower(args.Term)
	}

	reply.Success = false

	if args.Term == rm.broker.em.term {
		if rm.broker.state != Follower {
			rm.broker.em.becomeFollower(args.Term)
		}
		log.Printf("%s %d detects heartbeat or command from leaderid %d", rm.broker.state, rm.id, args.LeaderId)

		rm.broker.em.resetElectionTimer()

		// check if follower log contains previous entry (correct term and index)
		if args.PrevLogIndex == -1 || (args.PrevLogIndex < len(rm.log) && args.PrevLogTerm == rm.log[args.PrevLogIndex].Term) {
			log.Printf("%s %d contains previous entry, Accepts AE", rm.broker.state, rm.id)

			reply.Success = true

			logInsertIndex := args.PrevLogIndex + 1
			newEntriesIndex := 0

			// loop through log entries sent from leader to see where to start inserting
			for {
				// end of follower log reached meaning log is either shorter and must be appended upon
				// or follower log is up to date
				if logInsertIndex >= len(rm.log) || newEntriesIndex >= len(args.Entries) {
					break
				}
				// mismatch found, start appending from this index
				if rm.log[logInsertIndex].Term != args.Entries[newEntriesIndex].Term {
					break
				}
				logInsertIndex++
				newEntriesIndex++
			}

			// append missing entries to follower log
			if newEntriesIndex < len(args.Entries) {
				rm.log = append(rm.log[:logInsertIndex], args.Entries[newEntriesIndex:]...)
				log.Printf("%+v appended from index %d for term %d", args.Entries, newEntriesIndex, rm.log[newEntriesIndex].Term)
			}
			log.Printf("args.LeaderCommit > rm.commitIndex is %t", args.LeaderCommit > rm.commitIndex)
			log.Printf("args.LeaderCommit: %d    rm.commitIndex: %d", args.LeaderCommit, rm.commitIndex)

			if args.LeaderCommit > rm.commitIndex {
				// follower updates own commitindex here
				rm.commitIndex = min(args.LeaderCommit, len(rm.log)-1)
				log.Printf("%s %d updates commitIndex to %d", rm.broker.state, rm.id, rm.commitIndex)

				rm.newCommitReadyChan <- struct{}{}
			}

		} else {
			log.Printf("%s %d detects previous log mismatch, reject AE", rm.broker.state, rm.id)

			if args.PrevLogIndex >= len(rm.log) {
				reply.ConflictIndex = len(rm.log)
				reply.ConflictTerm = -1
			} else {
				reply.ConflictTerm = rm.log[args.PrevLogIndex].Term

				var i int
				for i = args.PrevLogIndex - 1; i >= 0; i-- {
					if rm.log[i].Term != reply.ConflictTerm {
						break
					}
				}
				reply.ConflictIndex = i + 1
			}
		}
	}

	reply.Term = rm.broker.em.term
	reply.Id = rm.id

	return nil
}

////////////////////////////////////////////////////////////////////
//THESE FUNCS ARE FOR TESTING AND DEPLOYMENT
////////////////////////////////////////////////////////////////////

func (rm *ReplicationModule) Submit(document string, command any) int {
	rm.broker.mu2.Lock()

	if rm.broker.state == Leader {
		submitIndex := len(rm.log)
		rm.log = append(rm.log, LogEntry{CRDTOperation: command, Term: rm.broker.em.term, Document: document})

		rm.broker.mu2.Unlock()
		rm.triggerAEChan <- struct{}{}
		return submitIndex
	}

	rm.broker.mu2.Unlock()
	return -1
}
