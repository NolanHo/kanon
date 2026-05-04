package protocol

type FileMeta struct {
	MtimeNS int64 `json:"mtime_ns"`
	Size    int64 `json:"size"`
}

type SnapshotResponse struct {
	Current int64               `json:"current"`
	Files   map[string]FileMeta `json:"files"`
}

type Event struct {
	Seq     int64  `json:"seq"`
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	MtimeNS *int64 `json:"mtime_ns,omitempty"`
	Size    *int64 `json:"size,omitempty"`
}

type ChangesResponse struct {
	Since   int64   `json:"since"`
	Current int64   `json:"current"`
	HasMore bool    `json:"has_more"`
	Events  []Event `json:"events"`
}

type HealthResponse struct {
	Status               string `json:"status"`
	ProtocolVersion      string `json:"protocol_version"`
	CurrentSeq           int64  `json:"current_seq"`
	WatcherRunning       bool   `json:"watcher_running"`
	WatcherRestarts      int64  `json:"watcher_restarts"`
	WatcherLastError     string `json:"watcher_last_error,omitempty"`
	WatcherLastErrorTS   string `json:"watcher_last_error_ts,omitempty"`
	LastReconcileTS      string `json:"last_reconcile_ts,omitempty"`
	LastReconcileFull    bool   `json:"last_reconcile_full"`
	LastReconcileMS      int64  `json:"last_reconcile_ms"`
	LastReconcileUpserts int    `json:"last_reconcile_upserts"`
	LastReconcileDeletes int    `json:"last_reconcile_deletes"`
	LastWatchEventTS     string `json:"last_watch_event_ts,omitempty"`
	TriggerQueueLen      int    `json:"trigger_queue_len"`
	TriggerQueueCap      int    `json:"trigger_queue_cap"`
}

const CurrentVersion = "v0alpha1"
