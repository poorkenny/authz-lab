package demo

// QueueMsg is one cache-invalidation event waiting between SQLite and the cache.
type QueueMsg struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
	Object  string `json:"object"`
	Rev     int64  `json:"rev"`
	Op      string `json:"op"`
	Status  string `json:"status"` // queued | delivered
}

// Queue is the FIFO delivery queue for invalidation events. Nothing is
// delivered on its own: the reviewer delivers the head of the queue with
// DeliverNext, so every run is deterministic.
type Queue struct {
	nextID  int
	pending []QueueMsg
	done    []QueueMsg // recent deliveries, oldest first
}

func (q *Queue) Enqueue(subject, object, op string, rev int64) QueueMsg {
	q.nextID++
	m := QueueMsg{ID: q.nextID, Subject: subject, Object: object, Rev: rev, Op: op, Status: "queued"}
	q.pending = append(q.pending, m)
	return m
}

// DeliverNext removes the head of the queue and returns it.
func (q *Queue) DeliverNext() (QueueMsg, bool) {
	if len(q.pending) == 0 {
		return QueueMsg{}, false
	}
	m := q.pending[0]
	q.pending = q.pending[1:]
	m.Status = "delivered"
	q.done = append(q.done, m)
	if len(q.done) > 6 {
		q.done = q.done[len(q.done)-6:]
	}
	return m, true
}

// Pending returns the latest queued message for subject/object and its
// 1-based position in the queue.
func (q *Queue) Pending(subject, object string) (QueueMsg, int, bool) {
	for i := len(q.pending) - 1; i >= 0; i-- {
		if m := q.pending[i]; m.Subject == subject && m.Object == object {
			return m, i + 1, true
		}
	}
	return QueueMsg{}, 0, false
}

func (q *Queue) Depth() int { return len(q.pending) }

// Snapshot lists recent deliveries followed by the queued messages (head first).
func (q *Queue) Snapshot() (delivered, queued []QueueMsg) {
	return append([]QueueMsg{}, q.done...), append([]QueueMsg{}, q.pending...)
}
