package issuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/logging"
)

type Server struct {
	Issuer       *auth.Issuer
	NodeTTL      time.Duration
	TokenTTL     time.Duration
	BootstrapIPs []string
	P2PPort      int

	// Scale-out (RunInstances or local process spawn)
	ScaleEnabled     bool
	LaunchTemplateID string
	Region           string
	SpawnMaxExtra    int
	ScaleCooldown    time.Duration
	ProjectTag       string
	// LocalDir, when set, replaces EC2 with scripts under that directory
	// (spawn.sh / terminate.sh / count.sh). LaunchTemplateID may be "local".
	LocalDir string

	mu            sync.Mutex
	certs         map[string]*auth.NodeCert // keyed by instance id or node id
	lastScale     time.Time
	lastDownscale time.Time
	scaleCount    int
	scaleReserved int // in-flight launches not yet reflected in inventory
	lastScaleBy   map[string]time.Time // per requester_id cooldown
	drainingID    string               // instance currently holding SoftLeave drain lock
	drainingSince time.Time

	// DownCooldown is the quiet period after a successful terminate before
	// another SoftLeave may take the drain lock. Keeps the mesh from
	// collapsing faster than transfers can rebalance.
	DownCooldown time.Duration

	// GlobalAPICooldown is a short anti-stampede pause between AWS/local
	// launch batches (not a mesh-size target). Defaults to 2s when unset.
	GlobalAPICooldown time.Duration
}

const drainLockTTL = 5 * time.Minute

func NewServer(iss *auth.Issuer) *Server {
	return &Server{
		Issuer:            iss,
		NodeTTL:           30 * 24 * time.Hour,
		TokenTTL:          24 * time.Hour,
		P2PPort:           21000,
		SpawnMaxExtra:     1,
		ScaleCooldown:     3 * time.Minute,
		DownCooldown:      2 * time.Minute,
		GlobalAPICooldown: 2 * time.Second,
		ProjectTag:        "indexus-aws",
		certs:             make(map[string]*auth.NodeCert),
		lastScaleBy:       make(map[string]time.Time),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "network_id": s.Issuer.NetworkID})
	})
	mux.HandleFunc("/v1/network/pubkey", s.getPubKey)
	mux.HandleFunc("/v1/issue/node", s.issueNode)
	mux.HandleFunc("/v1/issue/token", s.issueToken)
	mux.HandleFunc("/v1/cert", s.getCert)
	mux.HandleFunc("/v1/scale", s.scale)
	mux.HandleFunc("/v1/downscale", s.downscale)
	mux.HandleFunc("/v1/drain-lock", s.drainLock)
	mux.HandleFunc("/v1/drain-unlock", s.drainUnlock)
	return mux
}

// Serve runs the issuer until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context, addr string) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Warn("issuer shutdown", "err", err)
		}
	}()

	slog.Info("issuer listening",
		"addr", addr,
		"network", s.Issuer.NetworkID,
		"scale", s.ScaleEnabled,
		"launch_template", s.LaunchTemplateID)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) getPubKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"network_id": s.Issuer.NetworkID,
		"public_key": auth.PublicKeyBase64(s.Issuer.Key.Public),
	})
}

type issueNodeReq struct {
	NodeID     string `json:"node_id"`
	PubKey     string `json:"pub_key"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	InstanceID string `json:"instance_id"`
	// PreferNear is an optional node id / name whose XOR neighborhood
	// should be used when allocating a new node_id (scale-out relief).
	PreferNear string `json:"prefer_near"`
}

func (s *Server) issueNode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req issueNodeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.NodeID == "" {
		var name string
		var err error
		if req.PreferNear != "" {
			if target, decErr := encoding.BASE64.Decode(req.PreferNear); decErr == nil {
				// Keep ~16 bits of XOR proximity to the hot requester.
				name, err = encoding.BASE64.RandomNameNear(target, 20)
			}
		}
		if name == "" {
			name, err = encoding.BASE64.RandomName()
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		req.NodeID = name
	}
	if req.Port == 0 {
		req.Port = s.P2PPort
	}
	if req.IP == "" || req.PubKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ip and pub_key required"})
		return
	}
	cert, err := s.Issuer.SignNodeCert(req.NodeID, req.PubKey, req.IP, req.Port, s.NodeTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.certs[req.NodeID] = cert
	if req.InstanceID != "" {
		s.certs[req.InstanceID] = cert
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"cert": cert})
}

type issueTokenReq struct {
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

func (s *Server) issueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req issueTokenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.ClientID == "" {
		req.ClientID = "anonymous"
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{"read"}
	}
	tok, err := s.Issuer.SignClientToken(req.ClientID, req.Scopes, s.TokenTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	enc, err := auth.EncodeToken(tok)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": enc})
}

func (s *Server) getCert(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = r.URL.Query().Get("instance_id")
	}
	s.mu.Lock()
	cert := s.certs[id]
	s.mu.Unlock()
	if cert == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "cert not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cert":          cert,
		"bootstrap_ips": s.BootstrapIPs,
		"network_id":    s.Issuer.NetworkID,
	})
}

type scaleReq struct {
	RequesterID  string `json:"requester_id"`
	LocalInserts int64  `json:"local_inserts"`
	OwnedZones   int    `json:"owned_zones"`
	Reason       string `json:"reason"`
	Role         string `json:"role"`
	// SpawnCount requests up to N parallel instances in one scale call
	// (capped by remaining SpawnMaxExtra capacity). Default 1.
	SpawnCount int    `json:"spawn_count"`
	PreferNear string `json:"prefer_near"`
	// PreferNears are distinct XOR targets (one per instance). When empty the
	// issuer fans PreferNear into N adjacent slices.
	PreferNears []string `json:"prefer_nears"`
	// Legacy cert-preissue fields (optional)
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	NodeID     string `json:"node_id"`
	PubKey     string `json:"pub_key"`
	InstanceID string `json:"instance_id"`
}

func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req scaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	// Legacy path: pre-issue cert when IP+PubKey provided without launch.
	if req.IP != "" && req.PubKey != "" && !s.ScaleEnabled {
		s.legacyScaleCert(w, req)
		return
	}

	if !s.ScaleEnabled || (s.LaunchTemplateID == "" && s.LocalDir == "") {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "scale disabled or launch template missing",
		})
		return
	}

	apiCD := s.GlobalAPICooldown
	if apiCD <= 0 {
		apiCD = 2 * time.Second
	}
	// Per-requester cooldown uses ScaleCooldown (node-side is primary; this
	// stops one hot node from stampeding). Other requesters may proceed.
	requesterCD := s.ScaleCooldown
	if requesterCD <= 0 {
		requesterCD = time.Minute
	}

	s.mu.Lock()
	if !s.lastScale.IsZero() && time.Since(s.lastScale) < apiCD {
		s.mu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "api_cooldown"})
		return
	}
	if req.RequesterID != "" && s.lastScaleBy != nil {
		if t, ok := s.lastScaleBy[req.RequesterID]; ok && time.Since(t) < requesterCD {
			s.mu.Unlock()
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "requester_cooldown"})
			return
		}
	}
	running := s.countSpawnedLocked()
	// External terminates leave scaleCount high while inventory is empty.
	if s.scaleCount > running {
		slog.Info("reconcile scaleCount to live inventory",
			"was", s.scaleCount, "running", running)
		s.scaleCount = running
	}
	// Cap is SpawnMaxExtra only — not a target mesh size.
	remaining := s.SpawnMaxExtra - running - s.scaleReserved
	if remaining <= 0 {
		// Copy under the lock: another request's release (scaleReserved -= n)
		// races a read from the response map after Unlock.
		reserved := s.scaleReserved
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    "spawn max reached",
			"running":  running,
			"reserved": reserved,
			"max":      s.SpawnMaxExtra,
		})
		return
	}

	n := req.SpawnCount
	if n < 1 {
		n = 1
	}
	if n > remaining {
		n = remaining
	}
	if n > 3 {
		n = 3
	}
	s.scaleReserved += n
	s.mu.Unlock()

	preferNear := req.PreferNear
	if preferNear == "" {
		preferNear = req.RequesterID
	}
	targets := req.PreferNears
	if len(targets) == 0 {
		if built, err := encoding.BASE64.PreferNearTargets(preferNear, n); err == nil && len(built) > 0 {
			targets = built
		}
	}
	for len(targets) < n {
		targets = append(targets, preferNear)
	}
	targets = targets[:n]

	type spawnResult struct {
		id  string
		tag string
		err error
	}
	results := make([]spawnResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := targets[i]
			id, err := s.runInstancesNear(tag)
			results[i] = spawnResult{id: id, tag: tag, err: err}
		}(i)
	}
	wg.Wait()

	ids := make([]string, 0, n)
	tags := make([]string, 0, n)
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if r.id != "" {
			ids = append(ids, r.id)
			tags = append(tags, r.tag)
		}
	}

	s.mu.Lock()
	s.scaleReserved -= n
	if s.scaleReserved < 0 {
		s.scaleReserved = 0
	}
	if len(ids) > 0 {
		s.lastScale = time.Now()
		s.scaleCount += len(ids)
		if req.RequesterID != "" {
			if s.lastScaleBy == nil {
				s.lastScaleBy = make(map[string]time.Time)
			}
			s.lastScaleBy[req.RequesterID] = s.lastScale
		}
	}
	s.mu.Unlock()

	if len(ids) == 0 {
		errMsg := "spawn failed"
		if firstErr != nil {
			errMsg = firstErr.Error()
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": errMsg})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"action":         "spawned",
		"instance_id":    ids[0],
		"instance_ids":   ids,
		"spawned":        len(ids),
		"requester_id":   req.RequesterID,
		"prefer_near":    preferNear,
		"prefer_nears":   tags,
		"reason":         req.Reason,
		"local_inserts":  req.LocalInserts,
		"bootstrap_ips":  s.BootstrapIPs,
		"network_pubkey": auth.PublicKeyBase64(s.Issuer.Key.Public),
	})
}

func (s *Server) legacyScaleCert(w http.ResponseWriter, req scaleReq) {
	if req.Port == 0 {
		req.Port = s.P2PPort
	}
	if req.NodeID == "" {
		var name string
		if req.RequesterID != "" {
			if target, err := encoding.BASE64.Decode(req.RequesterID); err == nil {
				name, _ = encoding.BASE64.RandomNameNear(target, 20)
			}
		}
		if name == "" {
			name, _ = encoding.BASE64.RandomName()
		}
		req.NodeID = name
	}
	cert, err := s.Issuer.SignNodeCert(req.NodeID, req.PubKey, req.IP, req.Port, s.NodeTTL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.mu.Lock()
	s.certs[req.NodeID] = cert
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"action": "cert_issued",
		"cert":   cert,
		"note":   "scale launch disabled; cert only",
	})
}

func (s *Server) localMode() bool {
	return s.LocalDir != "" || s.LaunchTemplateID == "local"
}

func (s *Server) runInstances() (string, error) {
	return s.runInstancesNear("")
}

func (s *Server) runInstancesNear(preferNear string) (string, error) {
	if s.localMode() {
		return s.runLocalSpawnNear(preferNear)
	}
	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	// PreferNear is tagged so userdata can set INDEXUS_PREFER_NEAR for XOR placement.
	tags := fmt.Sprintf(
		"ResourceType=instance,Tags=[{Key=Name,Value=%s-spawned},{Key=Project,Value=%s},{Key=Role,Value=spawned}",
		s.ProjectTag, s.ProjectTag,
	)
	if preferNear != "" {
		// Escape is minimal: node names are url64 (no commas/braces).
		tags += fmt.Sprintf(",{Key=PreferNear,Value=%s}", preferNear)
	}
	tags += "]"
	args := []string{
		"ec2", "run-instances",
		"--region", region,
		"--launch-template", "LaunchTemplateId=" + s.LaunchTemplateID + ",Version=$Latest",
		"--tag-specifications", tags,
		"--query", "Instances[0].InstanceId",
		"--output", "text",
	}
	cmd := exec.Command("aws", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run-instances: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if id == "" || !strings.HasPrefix(id, "i-") {
		return "", fmt.Errorf("unexpected instance id %q", id)
	}
	slog.Info("instance spawned", "instance", id, "prefer_near", preferNear)
	return id, nil
}

func (s *Server) runLocalSpawn() (string, error) {
	return s.runLocalSpawnNear("")
}

func (s *Server) runLocalSpawnNear(preferNear string) (string, error) {
	dir := s.LocalDir
	if dir == "" {
		dir = "scripts/local"
	}
	script := filepath.Join(dir, "spawn.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(),
		"INDEXUS_LOCAL_DIR="+dir,
		"INDEXUS_PROJECT="+s.ProjectTag,
		"INDEXUS_BOOTSTRAP_IPS="+strings.Join(s.BootstrapIPs, ","),
		"INDEXUS_P2P_PORT="+fmt.Sprintf("%d", s.P2PPort),
		"INDEXUS_PREFER_NEAR="+preferNear,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("local spawn: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	// spawn.sh may print logs then the id on the last line.
	if lines := strings.Split(id, "\n"); len(lines) > 1 {
		id = strings.TrimSpace(lines[len(lines)-1])
	}
	if id == "" || !strings.HasPrefix(id, "local-") {
		return "", fmt.Errorf("unexpected local instance id %q (out=%q)", id, string(out))
	}
	slog.Info("local process spawned", "instance", id)
	return id, nil
}

func (s *Server) countSpawnedLocked() int {
	if s.localMode() {
		return s.countLocalSpawned()
	}
	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	cmd := exec.Command("aws", "ec2", "describe-instances",
		"--region", region,
		"--filters",
		"Name=tag:Name,Values="+s.ProjectTag+"-spawned",
		"Name=instance-state-name,Values=pending,running",
		"--query", "length(Reservations[].Instances[])",
		"--output", "text",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("cannot count spawned instances", "err", err, "output", string(out))
		return s.scaleCount
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

func (s *Server) countLocalSpawned() int {
	dir := s.LocalDir
	if dir == "" {
		dir = "scripts/local"
	}
	script := filepath.Join(dir, "count.sh")
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "INDEXUS_LOCAL_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("cannot count local processes", "err", err, "output", string(out))
		return s.scaleCount
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n)
	return n
}

type downscaleReq struct {
	RequesterID string `json:"requester_id"`
	InstanceID  string `json:"instance_id"`
	Reason      string `json:"reason"`
}

func (s *Server) drainLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cooldown := s.DownCooldown
	if cooldown <= 0 {
		cooldown = time.Minute
	}
	// Same holder renewing mid-SoftLeave must not be blocked by DownCooldown.
	renewing := s.drainingID == req.InstanceID
	if !renewing && !s.lastDownscale.IsZero() && time.Since(s.lastDownscale) < cooldown {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":          "downscale cooldown",
			"retry_after_ms": (cooldown - time.Since(s.lastDownscale)).Milliseconds(),
		})
		return
	}
	// Expire stale locks so a crashed SoftLeave cannot block the cluster forever.
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "another drain in progress",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.drainingID = req.InstanceID
	s.drainingSince = time.Now()
	writeJSON(w, http.StatusOK, map[string]any{"locked": true, "instance_id": req.InstanceID})
}

func (s *Server) drainUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"unlocked": true})
}

func (s *Server) downscale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downscaleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.InstanceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "instance_id required"})
		return
	}
	if !s.ScaleEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scale disabled"})
		return
	}

	if s.localMode() {
		s.terminateLocal(w, req)
		return
	}

	region := s.Region
	if region == "" {
		region = "eu-west-3"
	}
	// Safety: only terminate Role=spawned instances (never bootstrap).
	roleCmd := exec.Command("aws", "ec2", "describe-instances",
		"--region", region,
		"--instance-ids", req.InstanceID,
		"--query", "Reservations[0].Instances[0].Tags[?Key=='Role'].Value|[0]",
		"--output", "text",
	)
	roleOut, err := roleCmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(roleOut)) != "spawned" {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("refuse terminate: role=%q err=%v", strings.TrimSpace(string(roleOut)), err),
		})
		return
	}
	// Prefer holders of the drain lock (SoftLeave completed under lock).
	s.mu.Lock()
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "drain lock held by another instance",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.mu.Unlock()

	cmd := exec.Command("aws", "ec2", "terminate-instances",
		"--region", region,
		"--instance-ids", req.InstanceID,
		"--query", "TerminatingInstances[0].InstanceId",
		"--output", "text",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("%v: %s", err, string(out))})
		return
	}
	s.mu.Lock()
	if s.scaleCount > 0 {
		s.scaleCount--
	}
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	s.lastDownscale = time.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"action":       "terminated",
		"instance_id":  strings.TrimSpace(string(out)),
		"requester_id": req.RequesterID,
		"reason":       req.Reason,
	})
}

func (s *Server) terminateLocal(w http.ResponseWriter, req downscaleReq) {
	if !strings.HasPrefix(req.InstanceID, "local-") {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "refuse terminate: local mode only accepts local-* ids",
		})
		return
	}
	s.mu.Lock()
	if s.drainingID != "" && s.drainingID != req.InstanceID {
		if s.drainingSince.IsZero() || time.Since(s.drainingSince) > drainLockTTL {
			slog.Warn("expiring stale drain lock", "holder", s.drainingID)
			s.drainingID = ""
			s.drainingSince = time.Time{}
		} else {
			s.mu.Unlock()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "drain lock held by another instance",
				"draining_id": s.drainingID,
			})
			return
		}
	}
	s.mu.Unlock()

	dir := s.LocalDir
	if dir == "" {
		dir = "scripts/local"
	}
	script := filepath.Join(dir, "terminate.sh")
	cmd := exec.Command(script, req.InstanceID)
	cmd.Env = append(os.Environ(), "INDEXUS_LOCAL_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("local terminate: %v (%s)", err, strings.TrimSpace(string(out))),
		})
		return
	}
	s.mu.Lock()
	if s.scaleCount > 0 {
		s.scaleCount--
	}
	if s.drainingID == req.InstanceID {
		s.drainingID = ""
		s.drainingSince = time.Time{}
	}
	s.lastDownscale = time.Now()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"action":       "terminated",
		"instance_id":  req.InstanceID,
		"requester_id": req.RequesterID,
		"reason":       req.Reason,
	})
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}
