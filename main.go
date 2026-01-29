package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"golang.org/x/time/rate"
)

// =============================================================================
// CONFIGURATION
// =============================================================================

type Config struct {
	// Server settings
	Host       string
	APIPort    int
	EnableCORS bool

	// LiDAR settings
	LidarEnabled    bool
	LidarUDPPort    int
	LidarMaxPoints  int
	LidarVoxelSize  float64
	LidarTTLSeconds float64

	// Radar settings
	RadarEnabled    bool
	RadarTTLSeconds float64
	RadarMaxTargets int
	RadarMaxRange   float64

	// GStreamer settings
	GStreamerEnabled bool
	RTSPPort         int
	SRTPort          int
	GstLatency       int
	GstDropOnLatency bool
	GstBufferMode    int
	SRTLatency       int
	SRTPassphrase    string
	StreamStateFile  string

	// Security settings
	APIKey           string
	RateLimitEnabled bool
	RateLimitRPS     float64
	RateLimitBurst   int
	AllowedOrigins   []string

	// Monitoring settings
	MetricsEnabled      bool
	MetricsPort         int
	HealthCheckInterval time.Duration

	// Resource limits
	MaxConnections    int
	MaxDevicesPerType int
	ConnectionTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func DefaultConfig() *Config {
	return &Config{
		Host:       "0.0.0.0",
		APIPort:    8090,
		EnableCORS: true,

		LidarEnabled:    true,
		LidarUDPPort:    12345,
		LidarMaxPoints:  2000000,
		LidarVoxelSize:  0.02,
		LidarTTLSeconds: 2.0,

		RadarEnabled:    true,
		RadarTTLSeconds: 0.3,
		RadarMaxTargets: 10000,
		RadarMaxRange:   50.0,

		GStreamerEnabled: true,
		RTSPPort:         8560,
		SRTPort:          8561,
		GstLatency:       0,
		GstDropOnLatency: true,
		GstBufferMode:    4,
		SRTLatency:       50,
		SRTPassphrase:    "",
		StreamStateFile:  "streams_state.json",

		APIKey:           "",
		RateLimitEnabled: true,
		RateLimitRPS:     100,
		RateLimitBurst:   200,
		AllowedOrigins:   []string{"*"},

		MetricsEnabled:      true,
		MetricsPort:         9090,
		HealthCheckInterval: 10 * time.Second,

		MaxConnections:    1000,
		MaxDevicesPerType: 100,
		ConnectionTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ShutdownTimeout:   30 * time.Second,
	}
}

func LoadConfigFromEnv(cfg *Config) {
	if v := os.Getenv("HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("API_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.APIPort)
	}
	if v := os.Getenv("LIDAR_ENABLED"); v != "" {
		cfg.LidarEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("LIDAR_UDP_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.LidarUDPPort)
	}
	if v := os.Getenv("LIDAR_MAX_POINTS"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.LidarMaxPoints)
	}
	if v := os.Getenv("LIDAR_VOXEL_SIZE"); v != "" {
		fmt.Sscanf(v, "%f", &cfg.LidarVoxelSize)
	}
	if v := os.Getenv("RADAR_ENABLED"); v != "" {
		cfg.RadarEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("RADAR_TTL"); v != "" {
		fmt.Sscanf(v, "%f", &cfg.RadarTTLSeconds)
	}
	if v := os.Getenv("RADAR_MAX_RANGE"); v != "" {
		fmt.Sscanf(v, "%f", &cfg.RadarMaxRange)
	}
	if v := os.Getenv("GSTREAMER_ENABLED"); v != "" {
		cfg.GStreamerEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("RTSP_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.RTSPPort)
	}
	if v := os.Getenv("SRT_PORT"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.SRTPort)
	}
	if v := os.Getenv("SRT_LATENCY"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.SRTLatency)
	}
	if v := os.Getenv("GST_LATENCY"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.GstLatency)
	}
	if v := os.Getenv("SRT_PASSPHRASE"); v != "" {
		cfg.SRTPassphrase = v
	}
	if v := os.Getenv("STATE_FILE"); v != "" {
		cfg.StreamStateFile = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("RATE_LIMIT_ENABLED"); v != "" {
		cfg.RateLimitEnabled = v == "true" || v == "1"
	}
	if v := os.Getenv("RATE_LIMIT_RPS"); v != "" {
		fmt.Sscanf(v, "%f", &cfg.RateLimitRPS)
	}
	if v := os.Getenv("MAX_CONNECTIONS"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.MaxConnections)
	}
	if v := os.Getenv("MAX_DEVICES"); v != "" {
		fmt.Sscanf(v, "%d", &cfg.MaxDevicesPerType)
	}
}

// =============================================================================
// METRICS & MONITORING
// =============================================================================

type Metrics struct {
	// Connection metrics
	ActiveConnections   int64
	TotalConnections    int64
	FailedConnections   int64
	RejectedConnections int64

	// LiDAR metrics
	LidarDevices int64
	LidarPoints  int64
	LidarPackets int64
	LidarErrors  int64
	LidarBytesIn int64

	// Radar metrics
	RadarDevices int64
	RadarTargets int64
	RadarFrames  int64
	RadarErrors  int64
	RadarBytesIn int64

	// GStreamer metrics
	ActiveStreams  int64
	TotalStreams   int64
	StreamRestarts int64
	StreamErrors   int64

	// System metrics
	StartTime       time.Time
	LastHealthCheck time.Time
	HealthStatus    string

	mu sync.RWMutex
}

var globalMetrics = &Metrics{
	StartTime:    time.Now(),
	HealthStatus: "starting",
}

func (m *Metrics) IncrementConnections() {
	atomic.AddInt64(&m.ActiveConnections, 1)
	atomic.AddInt64(&m.TotalConnections, 1)
}

func (m *Metrics) DecrementConnections() {
	atomic.AddInt64(&m.ActiveConnections, -1)
}

func (m *Metrics) GetSnapshot() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return map[string]interface{}{
		"connections": map[string]int64{
			"active":   atomic.LoadInt64(&m.ActiveConnections),
			"total":    atomic.LoadInt64(&m.TotalConnections),
			"failed":   atomic.LoadInt64(&m.FailedConnections),
			"rejected": atomic.LoadInt64(&m.RejectedConnections),
		},
		"lidar": map[string]int64{
			"devices":  atomic.LoadInt64(&m.LidarDevices),
			"points":   atomic.LoadInt64(&m.LidarPoints),
			"packets":  atomic.LoadInt64(&m.LidarPackets),
			"errors":   atomic.LoadInt64(&m.LidarErrors),
			"bytes_in": atomic.LoadInt64(&m.LidarBytesIn),
		},
		"radar": map[string]int64{
			"devices":  atomic.LoadInt64(&m.RadarDevices),
			"targets":  atomic.LoadInt64(&m.RadarTargets),
			"frames":   atomic.LoadInt64(&m.RadarFrames),
			"errors":   atomic.LoadInt64(&m.RadarErrors),
			"bytes_in": atomic.LoadInt64(&m.RadarBytesIn),
		},
		"gstreamer": map[string]int64{
			"active_streams": atomic.LoadInt64(&m.ActiveStreams),
			"total_streams":  atomic.LoadInt64(&m.TotalStreams),
			"restarts":       atomic.LoadInt64(&m.StreamRestarts),
			"errors":         atomic.LoadInt64(&m.StreamErrors),
		},
		"system": map[string]interface{}{
			"uptime_seconds":    time.Since(m.StartTime).Seconds(),
			"health_status":     m.HealthStatus,
			"last_health_check": m.LastHealthCheck.Format(time.RFC3339),
			"goroutines":        runtime.NumGoroutine(),
			"memory_mb":         getMemoryUsageMB(),
		},
	}
}

func getMemoryUsageMB() float64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.Alloc) / 1024 / 1024
}

// =============================================================================
// RATE LIMITER & SECURITY
// =============================================================================

type RateLimiterPool struct {
	limiters sync.Map
	rps      float64
	burst    int
}

func NewRateLimiterPool(rps float64, burst int) *RateLimiterPool {
	return &RateLimiterPool{rps: rps, burst: burst}
}

func (p *RateLimiterPool) GetLimiter(key string) *rate.Limiter {
	if limiter, ok := p.limiters.Load(key); ok {
		return limiter.(*rate.Limiter)
	}
	limiter := rate.NewLimiter(rate.Limit(p.rps), p.burst)
	p.limiters.Store(key, limiter)
	return limiter
}

type ConnectionTracker struct {
	connections sync.Map
	count       int64
	maxConn     int
	mu          sync.Mutex
}

func NewConnectionTracker(maxConn int) *ConnectionTracker {
	return &ConnectionTracker{maxConn: maxConn}
}

func (ct *ConnectionTracker) Add(id string) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if int(ct.count) >= ct.maxConn {
		return false
	}
	ct.connections.Store(id, time.Now())
	ct.count++
	return true
}

func (ct *ConnectionTracker) Remove(id string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if _, ok := ct.connections.LoadAndDelete(id); ok {
		ct.count--
	}
}

func (ct *ConnectionTracker) Count() int {
	return int(atomic.LoadInt64(&ct.count))
}

// =============================================================================
// WEBSOCKET UPGRADER
// =============================================================================

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:   65535,
	WriteBufferSize:  65535,
	HandshakeTimeout: 10 * time.Second,
}

// =============================================================================
// LIDAR MODULE
// =============================================================================

const (
	LIDAR_SCAN_MSG_TYPE = 102
	LIDAR_POINT_SIZE    = 24
	LIDAR_MIN_DISTANCE  = 0.15
	LIDAR_MAX_DISTANCE  = 100.0
)

type LidarPoint struct {
	X, Y, Z   float64
	Intensity float64
	Timestamp int64 `json:"-"`
}

type VoxelKey struct {
	X, Y, Z int64
}

type VoxelGrid struct {
	occupied  map[VoxelKey]bool
	voxelSize float64
	mu        sync.RWMutex
}

func NewVoxelGrid(voxelSize float64) *VoxelGrid {
	return &VoxelGrid{
		occupied:  make(map[VoxelKey]bool),
		voxelSize: voxelSize,
	}
}

func (v *VoxelGrid) GetKey(x, y, z float64) VoxelKey {
	return VoxelKey{
		X: int64(math.Floor(x / v.voxelSize)),
		Y: int64(math.Floor(y / v.voxelSize)),
		Z: int64(math.Floor(z / v.voxelSize)),
	}
}

func (v *VoxelGrid) TryAdd(x, y, z float64) bool {
	key := v.GetKey(x, y, z)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.occupied[key] {
		return false
	}
	v.occupied[key] = true
	return true
}

func (v *VoxelGrid) Clear() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.occupied = make(map[VoxelKey]bool)
}

func (v *VoxelGrid) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.occupied)
}

type LidarPointCloud struct {
	Points       []LidarPoint
	mu           sync.RWMutex
	scanCount    int64
	pointCount   int64
	uniquePoints int64
	rejectedDups int64
	fps          float64
	lastUpdate   time.Time
	voxelGrid    *VoxelGrid
	voxelSize    float64
	maxPoints    int
	ttlSeconds   float64
	minX, maxX   float64
	minY, maxY   float64
	minZ, maxZ   float64
}

func NewLidarPointCloud(maxPoints int, voxelSize, ttlSeconds float64) *LidarPointCloud {
	return &LidarPointCloud{
		Points:     make([]LidarPoint, 0, maxPoints),
		voxelGrid:  NewVoxelGrid(voxelSize),
		voxelSize:  voxelSize,
		maxPoints:  maxPoints,
		ttlSeconds: ttlSeconds,
	}
}

func (c *LidarPointCloud) AddPoints(points []LidarPoint) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	nowNano := now.UnixNano()

	for i := range points {
		points[i].Timestamp = nowNano
	}

	addedCount := 0
	for _, p := range points {
		if c.voxelGrid.TryAdd(p.X, p.Y, p.Z) {
			c.Points = append(c.Points, p)
			addedCount++
			c.uniquePoints++

			if c.uniquePoints == 1 {
				c.minX, c.maxX = p.X, p.X
				c.minY, c.maxY = p.Y, p.Y
				c.minZ, c.maxZ = p.Z, p.Z
			} else {
				if p.X < c.minX {
					c.minX = p.X
				}
				if p.X > c.maxX {
					c.maxX = p.X
				}
				if p.Y < c.minY {
					c.minY = p.Y
				}
				if p.Y > c.maxY {
					c.maxY = p.Y
				}
				if p.Z < c.minZ {
					c.minZ = p.Z
				}
				if p.Z > c.maxZ {
					c.maxZ = p.Z
				}
			}
		} else {
			c.rejectedDups++
		}
	}

	if len(c.Points) > c.maxPoints {
		excess := len(c.Points) - c.maxPoints
		c.Points = c.Points[excess:]
	}

	c.scanCount++
	c.pointCount += int64(len(points))
	c.lastUpdate = now
	c.fps = float64(len(points)) / 0.1
}

func (c *LidarPointCloud) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Points = make([]LidarPoint, 0, c.maxPoints)
	c.scanCount = 0
	c.pointCount = 0
	c.uniquePoints = 0
	c.rejectedDups = 0
	c.fps = 0
	c.voxelGrid.Clear()
}

func (c *LidarPointCloud) GetStats() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]interface{}{
		"point_count":   len(c.Points),
		"total_points":  c.pointCount,
		"unique_points": c.uniquePoints,
		"rejected_dups": c.rejectedDups,
		"scan_count":    c.scanCount,
		"fps":           c.fps,
		"last_update":   c.lastUpdate.Format(time.RFC3339),
		"is_live":       time.Since(c.lastUpdate) < 2*time.Second,
		"voxel_size":    c.voxelSize,
		"max_points":    c.maxPoints,
		"bounds": map[string]float64{
			"min_x": c.minX, "max_x": c.maxX,
			"min_y": c.minY, "max_y": c.maxY,
			"min_z": c.minZ, "max_z": c.maxZ,
		},
	}
}

type LidarDevice struct {
	DeviceID    string
	Cloud       *LidarPointCloud
	Conn        *websocket.Conn
	LastActive  time.Time
	IsConnected bool
	RemoteAddr  string
	ConnectedAt time.Time
	mu          sync.RWMutex
}

type LidarModule struct {
	config       *Config
	devices      map[string]*LidarDevice
	devicesMu    sync.RWMutex
	defaultCloud *LidarPointCloud
	udpConn      *net.UDPConn
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func NewLidarModule(config *Config) *LidarModule {
	ctx, cancel := context.WithCancel(context.Background())
	return &LidarModule{
		config:       config,
		devices:      make(map[string]*LidarDevice),
		defaultCloud: NewLidarPointCloud(config.LidarMaxPoints, config.LidarVoxelSize, config.LidarTTLSeconds),
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (m *LidarModule) Start() error {
	if !m.config.LidarEnabled {
		log.Println("📡 LiDAR module disabled")
		return nil
	}

	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", m.config.LidarUDPPort))
	if err != nil {
		return fmt.Errorf("failed to resolve UDP address: %v", err)
	}

	m.udpConn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on UDP: %v", err)
	}

	m.wg.Add(1)
	go m.receiveUDP()

	m.wg.Add(1)
	go m.cleanupStaleDevices()

	log.Printf("📡 LiDAR module started (UDP: %d, Voxel: %.2fcm)", m.config.LidarUDPPort, m.config.LidarVoxelSize*100)
	return nil
}

func (m *LidarModule) Stop() {
	m.cancel()
	if m.udpConn != nil {
		m.udpConn.Close()
	}
	m.wg.Wait()
	log.Println("📡 LiDAR module stopped")
}

func (m *LidarModule) receiveUDP() {
	defer m.wg.Done()
	buffer := make([]byte, 65535)

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		m.udpConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := m.udpConn.ReadFromUDP(buffer)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if m.ctx.Err() != nil {
				return
			}
			atomic.AddInt64(&globalMetrics.LidarErrors, 1)
			continue
		}

		atomic.AddInt64(&globalMetrics.LidarBytesIn, int64(n))
		atomic.AddInt64(&globalMetrics.LidarPackets, 1)

		points := m.parseLidarPacket(buffer[:n])
		if len(points) > 0 {
			m.defaultCloud.AddPoints(points)
			atomic.StoreInt64(&globalMetrics.LidarPoints, int64(len(m.defaultCloud.Points)))
		}
	}
}

func (m *LidarModule) parseLidarPacket(data []byte) []LidarPoint {
	if len(data) < 8 {
		return nil
	}

	msgType := binary.LittleEndian.Uint32(data[0:4])
	if msgType != LIDAR_SCAN_MSG_TYPE {
		return nil
	}

	payload := data[8:]
	if len(payload) < 16 {
		return nil
	}

	validPointsNum := binary.LittleEndian.Uint32(payload[12:16])
	if validPointsNum > 500 {
		validPointsNum = 500
	}

	pointData := payload[16:]
	maxPossible := uint32(len(pointData) / LIDAR_POINT_SIZE)
	if validPointsNum > maxPossible {
		validPointsNum = maxPossible
	}

	points := make([]LidarPoint, 0, validPointsNum)
	for i := uint32(0); i < validPointsNum; i++ {
		offset := int(i) * LIDAR_POINT_SIZE
		if offset+LIDAR_POINT_SIZE > len(pointData) {
			break
		}

		x := math.Float32frombits(binary.LittleEndian.Uint32(pointData[offset : offset+4]))
		y := math.Float32frombits(binary.LittleEndian.Uint32(pointData[offset+4 : offset+8]))
		z := math.Float32frombits(binary.LittleEndian.Uint32(pointData[offset+8 : offset+12]))
		intensity := math.Float32frombits(binary.LittleEndian.Uint32(pointData[offset+12 : offset+16]))

		if math.IsNaN(float64(x)) || math.IsNaN(float64(y)) || math.IsNaN(float64(z)) ||
			math.IsInf(float64(x), 0) || math.IsInf(float64(y), 0) || math.IsInf(float64(z), 0) {
			continue
		}

		distance := math.Sqrt(float64(x*x + y*y + z*z))
		if distance < LIDAR_MIN_DISTANCE || distance > LIDAR_MAX_DISTANCE {
			continue
		}

		points = append(points, LidarPoint{
			X:         float64(x),
			Y:         float64(y),
			Z:         float64(z),
			Intensity: float64(intensity),
		})
	}

	return points
}

func (m *LidarModule) HandleWebSocketIngest(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ LiDAR WebSocket upgrade failed: %v", err)
		atomic.AddInt64(&globalMetrics.FailedConnections, 1)
		return
	}
	defer conn.Close()

	globalMetrics.IncrementConnections()
	defer globalMetrics.DecrementConnections()

	var deviceID string
	log.Printf("🔗 LiDAR WebSocket connection from %s", r.RemoteAddr)

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("❌ LiDAR WebSocket error for device %s: %v", deviceID, err)
			}
			break
		}

		if messageType == websocket.TextMessage {
			var msg map[string]interface{}
			if json.Unmarshal(data, &msg) == nil {
				msgType, _ := msg["type"].(string)
				if msgType == "register" {
					deviceID, _ = msg["device_id"].(string)
					if deviceID == "" {
						if dataObj, ok := msg["data"].(map[string]interface{}); ok {
							deviceID, _ = dataObj["device_id"].(string)
						}
					}
					if deviceID == "" {
						deviceID = fmt.Sprintf("lidar_%d", time.Now().UnixNano())
					}

					m.registerDevice(deviceID, conn, r.RemoteAddr)

					ack := map[string]interface{}{
						"type":      "registered",
						"device_id": deviceID,
						"timestamp": time.Now().Format(time.RFC3339),
					}
					ackData, _ := json.Marshal(ack)
					conn.WriteMessage(websocket.TextMessage, ackData)
				}
			}
			continue
		}

		if messageType == websocket.BinaryMessage && len(data) > 0 {
			atomic.AddInt64(&globalMetrics.LidarBytesIn, int64(len(data)))
			atomic.AddInt64(&globalMetrics.LidarPackets, 1)
			m.processLidarPacket(data, deviceID)
		}
	}

	if deviceID != "" {
		m.disconnectDevice(deviceID)
	}
}

func (m *LidarModule) registerDevice(deviceID string, conn *websocket.Conn, remoteAddr string) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()

	if _, exists := m.devices[deviceID]; !exists {
		m.devices[deviceID] = &LidarDevice{
			DeviceID:    deviceID,
			Cloud:       NewLidarPointCloud(m.config.LidarMaxPoints, m.config.LidarVoxelSize, m.config.LidarTTLSeconds),
			Conn:        conn,
			LastActive:  time.Now(),
			IsConnected: true,
			RemoteAddr:  remoteAddr,
			ConnectedAt: time.Now(),
		}
		atomic.AddInt64(&globalMetrics.LidarDevices, 1)
		log.Printf("✅ LiDAR device registered: %s", deviceID)
	} else {
		device := m.devices[deviceID]
		device.mu.Lock()
		device.Conn = conn
		device.IsConnected = true
		device.LastActive = time.Now()
		device.RemoteAddr = remoteAddr
		device.mu.Unlock()
		log.Printf("🔄 LiDAR device reconnected: %s", deviceID)
	}
}

func (m *LidarModule) disconnectDevice(deviceID string) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()

	if device, exists := m.devices[deviceID]; exists {
		device.mu.Lock()
		device.IsConnected = false
		device.Conn = nil
		device.mu.Unlock()
		log.Printf("🔌 LiDAR device disconnected: %s", deviceID)
	}
}

func (m *LidarModule) processLidarPacket(data []byte, deviceID string) {
	points := m.parseLidarPacket(data)
	if len(points) == 0 {
		return
	}

	var cloud *LidarPointCloud
	if deviceID != "" {
		m.devicesMu.RLock()
		if device, exists := m.devices[deviceID]; exists {
			cloud = device.Cloud
			device.LastActive = time.Now()
		}
		m.devicesMu.RUnlock()
	}
	if cloud == nil {
		cloud = m.defaultCloud
	}

	cloud.AddPoints(points)
	atomic.StoreInt64(&globalMetrics.LidarPoints, int64(len(cloud.Points)))
}

func (m *LidarModule) cleanupStaleDevices() {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.devicesMu.Lock()
			for id, device := range m.devices {
				device.mu.RLock()
				isStale := time.Since(device.LastActive) > 30*time.Minute && !device.IsConnected
				device.mu.RUnlock()
				if isStale {
					delete(m.devices, id)
					atomic.AddInt64(&globalMetrics.LidarDevices, -1)
					log.Printf("🗑️ Removed stale LiDAR device: %s", id)
				}
			}
			m.devicesMu.Unlock()
		}
	}
}

func (m *LidarModule) GetCloud(deviceID string) *LidarPointCloud {
	if deviceID == "" || deviceID == "default" {
		m.devicesMu.RLock()
		for _, device := range m.devices {
			device.mu.RLock()
			isLive := device.IsConnected && len(device.Cloud.Points) > 0
			device.mu.RUnlock()
			if isLive {
				m.devicesMu.RUnlock()
				return device.Cloud
			}
		}
		m.devicesMu.RUnlock()
		return m.defaultCloud
	}

	m.devicesMu.RLock()
	if device, exists := m.devices[deviceID]; exists {
		m.devicesMu.RUnlock()
		return device.Cloud
	}
	m.devicesMu.RUnlock()
	return m.defaultCloud
}

func (m *LidarModule) GetDevices() []map[string]interface{} {
	devices := []map[string]interface{}{}

	m.defaultCloud.mu.RLock()
	devices = append(devices, map[string]interface{}{
		"id":          "default",
		"name":        "Local UDP",
		"type":        "local",
		"is_live":     time.Since(m.defaultCloud.lastUpdate) < 2*time.Second,
		"point_count": len(m.defaultCloud.Points),
		"last_update": m.defaultCloud.lastUpdate.Format(time.RFC3339),
	})
	m.defaultCloud.mu.RUnlock()

	m.devicesMu.RLock()
	for id, device := range m.devices {
		device.mu.RLock()
		device.Cloud.mu.RLock()
		devices = append(devices, map[string]interface{}{
			"id":           id,
			"name":         id,
			"type":         "remote",
			"is_connected": device.IsConnected,
			"is_live":      time.Since(device.Cloud.lastUpdate) < 2*time.Second,
			"point_count":  len(device.Cloud.Points),
			"last_update":  device.Cloud.lastUpdate.Format(time.RFC3339),
			"remote_addr":  device.RemoteAddr,
			"connected_at": device.ConnectedAt.Format(time.RFC3339),
		})
		device.Cloud.mu.RUnlock()
		device.mu.RUnlock()
	}
	m.devicesMu.RUnlock()

	return devices
}

func (m *LidarModule) ClearCloud(deviceID string) {
	if deviceID == "" || deviceID == "all" {
		m.defaultCloud.Clear()
		m.devicesMu.RLock()
		for _, device := range m.devices {
			device.Cloud.Clear()
		}
		m.devicesMu.RUnlock()
		log.Println("🗑️ Cleared all LiDAR point clouds")
	} else {
		m.devicesMu.RLock()
		if device, exists := m.devices[deviceID]; exists {
			device.Cloud.Clear()
			log.Printf("🗑️ Cleared LiDAR point cloud for device: %s", deviceID)
		}
		m.devicesMu.RUnlock()
	}
}

// =============================================================================
// RADAR MODULE
// =============================================================================

type RadarTarget struct {
	X         float64   `json:"x"`
	Y         float64   `json:"y"`
	Z         float64   `json:"z,omitempty"`
	Range     float64   `json:"range"`
	Velocity  float64   `json:"velocity"`
	Azimuth   float64   `json:"azimuth"`
	RCS       float64   `json:"rcs"`
	SNR       float64   `json:"snr"`
	Timestamp time.Time `json:"timestamp"`
}

type RadarFrame struct {
	DeviceID    string        `json:"device_id"`
	FrameID     uint64        `json:"frame_id"`
	Timestamp   time.Time     `json:"timestamp"`
	TargetCount int           `json:"target_count"`
	Targets     []RadarTarget `json:"targets"`
}

type RadarDeviceData struct {
	DeviceID    string
	Targets     []RadarTarget
	FrameCount  uint64
	TargetCount uint64
	LastUpdate  time.Time
	LastFrame   *RadarFrame
	FPS         float64
	IsConnected bool
	Conn        *websocket.Conn
	RemoteAddr  string
	ConnectedAt time.Time
	frameTimes  []time.Time
	mu          sync.RWMutex
}

type RadarModule struct {
	config      *Config
	devices     map[string]*RadarDeviceData
	devicesMu   sync.RWMutex
	defaultData *RadarDeviceData
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

func NewRadarModule(config *Config) *RadarModule {
	ctx, cancel := context.WithCancel(context.Background())
	return &RadarModule{
		config:  config,
		devices: make(map[string]*RadarDeviceData),
		defaultData: &RadarDeviceData{
			DeviceID: "default",
			Targets:  make([]RadarTarget, 0, config.RadarMaxTargets),
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *RadarModule) Start() error {
	if !m.config.RadarEnabled {
		log.Println("🎯 Radar module disabled")
		return nil
	}

	m.wg.Add(1)
	go m.cleanupRoutine()

	log.Printf("🎯 Radar module started (TTL: %.1fs, Range: %.1fm)", m.config.RadarTTLSeconds, m.config.RadarMaxRange)
	return nil
}

func (m *RadarModule) Stop() {
	m.cancel()
	m.wg.Wait()
	log.Println("🎯 Radar module stopped")
}

func (m *RadarModule) cleanupRoutine() {
	defer m.wg.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-time.Duration(m.config.RadarTTLSeconds * float64(time.Second)))

			m.defaultData.mu.Lock()
			newTargets := make([]RadarTarget, 0, len(m.defaultData.Targets))
			for _, t := range m.defaultData.Targets {
				if t.Timestamp.After(cutoff) {
					newTargets = append(newTargets, t)
				}
			}
			m.defaultData.Targets = newTargets
			m.defaultData.mu.Unlock()

			m.devicesMu.RLock()
			for _, device := range m.devices {
				device.mu.Lock()
				newTargets := make([]RadarTarget, 0, len(device.Targets))
				for _, t := range device.Targets {
					if t.Timestamp.After(cutoff) {
						newTargets = append(newTargets, t)
					}
				}
				device.Targets = newTargets
				device.mu.Unlock()
			}
			m.devicesMu.RUnlock()

			atomic.StoreInt64(&globalMetrics.RadarTargets, int64(len(m.defaultData.Targets)))
		}
	}
}

func (m *RadarModule) HandleWebSocketIngest(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ Radar WebSocket upgrade failed: %v", err)
		atomic.AddInt64(&globalMetrics.FailedConnections, 1)
		return
	}
	defer conn.Close()

	globalMetrics.IncrementConnections()
	defer globalMetrics.DecrementConnections()

	var deviceID string
	log.Printf("🔗 Radar WebSocket connection from %s", r.RemoteAddr)

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("❌ Radar WebSocket error for device %s: %v", deviceID, err)
			}
			break
		}

		if messageType == websocket.TextMessage {
			var msg map[string]interface{}
			if json.Unmarshal(data, &msg) == nil {
				msgType, _ := msg["type"].(string)

				switch msgType {
				case "register":
					deviceID, _ = msg["device_id"].(string)
					if deviceID == "" {
						deviceID = fmt.Sprintf("radar_%d", time.Now().UnixNano())
					}

					m.registerDevice(deviceID, conn, r.RemoteAddr)

					ack := map[string]interface{}{
						"type":      "registered",
						"device_id": deviceID,
						"timestamp": time.Now().Format(time.RFC3339),
					}
					ackData, _ := json.Marshal(ack)
					conn.WriteMessage(websocket.TextMessage, ackData)

				case "frame":
					atomic.AddInt64(&globalMetrics.RadarBytesIn, int64(len(data)))
					if frameData, ok := msg["data"].(map[string]interface{}); ok {
						frame := m.parseFrameFromJSON(deviceID, frameData)
						if frame != nil {
							m.addFrame(deviceID, frame)
						}
					}
				}
			}
			continue
		}

		if messageType == websocket.BinaryMessage && len(data) > 0 {
			atomic.AddInt64(&globalMetrics.RadarBytesIn, int64(len(data)))
			var frame RadarFrame
			if err := json.Unmarshal(data, &frame); err == nil {
				frame.DeviceID = deviceID
				m.addFrame(deviceID, &frame)
			}
		}
	}

	if deviceID != "" {
		m.disconnectDevice(deviceID)
	}
}

func (m *RadarModule) registerDevice(deviceID string, conn *websocket.Conn, remoteAddr string) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()

	if _, exists := m.devices[deviceID]; !exists {
		m.devices[deviceID] = &RadarDeviceData{
			DeviceID:    deviceID,
			Targets:     make([]RadarTarget, 0, m.config.RadarMaxTargets),
			IsConnected: true,
			Conn:        conn,
			RemoteAddr:  remoteAddr,
			ConnectedAt: time.Now(),
		}
		atomic.AddInt64(&globalMetrics.RadarDevices, 1)
		log.Printf("✅ Radar device registered: %s", deviceID)
	} else {
		device := m.devices[deviceID]
		device.mu.Lock()
		device.IsConnected = true
		device.Conn = conn
		device.RemoteAddr = remoteAddr
		device.mu.Unlock()
		log.Printf("🔄 Radar device reconnected: %s", deviceID)
	}
}

func (m *RadarModule) disconnectDevice(deviceID string) {
	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()

	if device, exists := m.devices[deviceID]; exists {
		device.mu.Lock()
		device.IsConnected = false
		device.Conn = nil
		device.mu.Unlock()
		log.Printf("🔌 Radar device disconnected: %s", deviceID)
	}
}

func (m *RadarModule) getOrCreateDevice(deviceID string) *RadarDeviceData {
	if deviceID == "" || deviceID == "default" {
		return m.defaultData
	}

	m.devicesMu.Lock()
	defer m.devicesMu.Unlock()

	if device, exists := m.devices[deviceID]; exists {
		return device
	}

	device := &RadarDeviceData{
		DeviceID: deviceID,
		Targets:  make([]RadarTarget, 0, m.config.RadarMaxTargets),
	}
	m.devices[deviceID] = device
	atomic.AddInt64(&globalMetrics.RadarDevices, 1)
	return device
}

func (m *RadarModule) addFrame(deviceID string, frame *RadarFrame) {
	device := m.getOrCreateDevice(deviceID)

	device.mu.Lock()
	defer device.mu.Unlock()

	now := time.Now()

	for i := range frame.Targets {
		frame.Targets[i].Timestamp = now
	}

	device.Targets = append(device.Targets, frame.Targets...)

	if len(device.Targets) > m.config.RadarMaxTargets {
		device.Targets = device.Targets[len(device.Targets)-m.config.RadarMaxTargets:]
	}

	device.FrameCount++
	device.TargetCount += uint64(len(frame.Targets))
	device.LastUpdate = now
	device.LastFrame = frame

	device.frameTimes = append(device.frameTimes, now)
	if len(device.frameTimes) > 30 {
		device.frameTimes = device.frameTimes[1:]
	}
	if len(device.frameTimes) >= 2 {
		duration := device.frameTimes[len(device.frameTimes)-1].Sub(device.frameTimes[0])
		if duration > 0 {
			device.FPS = float64(len(device.frameTimes)-1) / duration.Seconds()
		}
	}

	atomic.AddInt64(&globalMetrics.RadarFrames, 1)
	atomic.StoreInt64(&globalMetrics.RadarTargets, int64(len(device.Targets)))
}

func (m *RadarModule) parseFrameFromJSON(deviceID string, data map[string]interface{}) *RadarFrame {
	frame := &RadarFrame{
		DeviceID:  deviceID,
		Timestamp: time.Now(),
		Targets:   make([]RadarTarget, 0),
	}

	if targets, ok := data["targets"].([]interface{}); ok {
		for _, t := range targets {
			if targetMap, ok := t.(map[string]interface{}); ok {
				target := RadarTarget{
					X:        getFloat(targetMap, "x"),
					Y:        getFloat(targetMap, "y"),
					Z:        getFloat(targetMap, "z"),
					Range:    getFloat(targetMap, "range"),
					Velocity: getFloat(targetMap, "velocity"),
					Azimuth:  getFloat(targetMap, "azimuth"),
					RCS:      getFloat(targetMap, "rcs"),
					SNR:      getFloat(targetMap, "snr"),
				}

				if target.Range == 0 && (target.X != 0 || target.Y != 0) {
					target.Range = math.Sqrt(target.X*target.X + target.Y*target.Y)
				}

				if target.Azimuth == 0 && (target.X != 0 || target.Y != 0) {
					target.Azimuth = math.Atan2(target.X, target.Y) * 180 / math.Pi
				}

				frame.Targets = append(frame.Targets, target)
			}
		}
	}

	frame.TargetCount = len(frame.Targets)
	return frame
}

func getFloat(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

func (m *RadarModule) GetDevice(deviceID string) *RadarDeviceData {
	if deviceID == "" {
		m.devicesMu.RLock()
		defer m.devicesMu.RUnlock()

		for _, device := range m.devices {
			if device.IsConnected && len(device.Targets) > 0 {
				return device
			}
		}
		for _, device := range m.devices {
			if len(device.Targets) > 0 {
				return device
			}
		}
		return m.defaultData
	}

	m.devicesMu.RLock()
	device, exists := m.devices[deviceID]
	m.devicesMu.RUnlock()

	if exists {
		return device
	}
	return m.defaultData
}

func (m *RadarModule) GetDevices() []map[string]interface{} {
	devices := []map[string]interface{}{}

	m.defaultData.mu.RLock()
	devices = append(devices, map[string]interface{}{
		"id":           "default",
		"name":         "Default (Local)",
		"type":         "local",
		"is_connected": false,
		"is_live":      time.Since(m.defaultData.LastUpdate) < 5*time.Second,
		"target_count": len(m.defaultData.Targets),
		"frame_count":  m.defaultData.FrameCount,
		"fps":          m.defaultData.FPS,
		"last_update":  m.defaultData.LastUpdate.Format(time.RFC3339),
	})
	m.defaultData.mu.RUnlock()

	m.devicesMu.RLock()
	for id, device := range m.devices {
		device.mu.RLock()
		devices = append(devices, map[string]interface{}{
			"id":           id,
			"name":         id,
			"type":         "remote",
			"is_connected": device.IsConnected,
			"is_live":      time.Since(device.LastUpdate) < 5*time.Second,
			"target_count": len(device.Targets),
			"frame_count":  device.FrameCount,
			"fps":          device.FPS,
			"last_update":  device.LastUpdate.Format(time.RFC3339),
			"remote_addr":  device.RemoteAddr,
			"connected_at": device.ConnectedAt.Format(time.RFC3339),
		})
		device.mu.RUnlock()
	}
	m.devicesMu.RUnlock()

	return devices
}

func (m *RadarModule) GetStats(deviceID string) map[string]interface{} {
	device := m.GetDevice(deviceID)

	device.mu.RLock()
	defer device.mu.RUnlock()

	return map[string]interface{}{
		"device_id":     device.DeviceID,
		"target_count":  len(device.Targets),
		"total_targets": device.TargetCount,
		"frame_count":   device.FrameCount,
		"fps":           device.FPS,
		"last_update":   device.LastUpdate.Format(time.RFC3339),
		"is_live":       time.Since(device.LastUpdate) < 5*time.Second,
		"is_connected":  device.IsConnected,
		"ttl_seconds":   m.config.RadarTTLSeconds,
		"max_range":     m.config.RadarMaxRange,
	}
}

func (m *RadarModule) ClearData(deviceID string) {
	if deviceID == "" || deviceID == "all" {
		m.defaultData.mu.Lock()
		m.defaultData.Targets = make([]RadarTarget, 0, m.config.RadarMaxTargets)
		m.defaultData.FrameCount = 0
		m.defaultData.TargetCount = 0
		m.defaultData.mu.Unlock()

		m.devicesMu.Lock()
		for _, device := range m.devices {
			device.mu.Lock()
			device.Targets = make([]RadarTarget, 0, m.config.RadarMaxTargets)
			device.FrameCount = 0
			device.TargetCount = 0
			device.mu.Unlock()
		}
		m.devicesMu.Unlock()

		log.Println("🗑️ Cleared all radar data")
	} else {
		device := m.GetDevice(deviceID)
		device.mu.Lock()
		device.Targets = make([]RadarTarget, 0, m.config.RadarMaxTargets)
		device.FrameCount = 0
		device.TargetCount = 0
		device.mu.Unlock()
		log.Printf("🗑️ Cleared radar data for device: %s", deviceID)
	}
}

// =============================================================================
// GSTREAMER MODULE - WITH HEVC SUPPORT
// =============================================================================

type StreamType string

// VideoCodec represents the video codec type
type VideoCodec string

const (
	StreamTypeRTSP    StreamType = "rtsp"
	StreamTypeSRT     StreamType = "srt"
	StreamTypeSRTCall StreamType = "srt_call"

	// Video codec types
	CodecH264 VideoCodec = "h264"
	CodecHEVC VideoCodec = "hevc"
	CodecAuto VideoCodec = "auto"
)

// normalizeCodec normalizes codec string to standard format
func normalizeCodec(codec string) VideoCodec {
	switch strings.ToLower(codec) {
	case "hevc", "h265", "h.265":
		return CodecHEVC
	case "h264", "h.264", "avc":
		return CodecH264
	case "auto", "":
		return CodecAuto
	default:
		return CodecH264 // Default to H.264 for backward compatibility
	}
}

type StreamState struct {
	Name       string     `json:"name"`
	SourceURL  string     `json:"source_url,omitempty"`
	StreamType StreamType `json:"stream_type"`
	Codec      string     `json:"codec,omitempty"` // NEW: Video codec
	Port       int        `json:"port,omitempty"`
	CreatedAt  string     `json:"created_at"`
}

type PersistentState struct {
	Streams   []StreamState `json:"streams"`
	UpdatedAt string        `json:"updated_at"`
}

type streamPath struct {
	mu        sync.RWMutex
	name      string
	stream    *gortsplib.ServerStream
	publisher *gortsplib.ServerSession
	readers   int
}

func newStreamPath(name string) *streamPath {
	return &streamPath{name: name}
}

func (p *streamPath) hasPublisher() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.publisher != nil
}

func (p *streamPath) getStream() *gortsplib.ServerStream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stream
}

func (p *streamPath) setPublisher(sess *gortsplib.ServerSession, stream *gortsplib.ServerStream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publisher = sess
	p.stream = stream
}

func (p *streamPath) clearPublisher(sess *gortsplib.ServerSession) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.publisher == sess {
		p.publisher = nil
		if p.stream != nil {
			p.stream.Close()
			p.stream = nil
		}
	}
}

func (p *streamPath) addReader() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readers++
}

func (p *streamPath) removeReader() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readers > 0 {
		p.readers--
	}
}

type rtspHandler struct {
	server     *gortsplib.Server
	paths      map[string]*streamPath
	pathsMu    sync.RWMutex
	sessions   map[*gortsplib.ServerSession]*sessionInfo
	sessionsMu sync.RWMutex
}

type sessionInfo struct {
	pathName    string
	isPublisher bool
}

func newRTSPHandler() *rtspHandler {
	return &rtspHandler{
		paths:    make(map[string]*streamPath),
		sessions: make(map[*gortsplib.ServerSession]*sessionInfo),
	}
}

func (h *rtspHandler) getOrCreatePath(name string) *streamPath {
	h.pathsMu.Lock()
	defer h.pathsMu.Unlock()
	if p, ok := h.paths[name]; ok {
		return p
	}
	p := newStreamPath(name)
	h.paths[name] = p
	return p
}

func (h *rtspHandler) getPath(name string) *streamPath {
	h.pathsMu.RLock()
	defer h.pathsMu.RUnlock()
	return h.paths[name]
}

func (h *rtspHandler) removePath(name string) {
	h.pathsMu.Lock()
	defer h.pathsMu.Unlock()
	delete(h.paths, name)
}

func (h *rtspHandler) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	log.Printf("📡 RTSP conn opened: %v", ctx.Conn.NetConn().RemoteAddr())
	globalMetrics.IncrementConnections()
}

func (h *rtspHandler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	log.Printf("📡 RTSP conn closed: %v", ctx.Conn.NetConn().RemoteAddr())
	globalMetrics.DecrementConnections()
}

func (h *rtspHandler) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	h.sessionsMu.Lock()
	h.sessions[ctx.Session] = &sessionInfo{}
	h.sessionsMu.Unlock()
}

func (h *rtspHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	h.sessionsMu.Lock()
	info := h.sessions[ctx.Session]
	delete(h.sessions, ctx.Session)
	h.sessionsMu.Unlock()

	if info != nil && info.pathName != "" {
		if p := h.getPath(info.pathName); p != nil {
			if info.isPublisher {
				p.clearPublisher(ctx.Session)
				log.Printf("📤 RTSP publisher disconnected: /%s", info.pathName)
			} else {
				p.removeReader()
			}
		}
	}
}

func (h *rtspHandler) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	pathName := strings.TrimPrefix(ctx.Path, "/")
	p := h.getPath(pathName)
	if p == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	stream := p.getStream()
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

func (h *rtspHandler) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	pathName := strings.TrimPrefix(ctx.Path, "/")
	log.Printf("📢 RTSP ANNOUNCE /%s", pathName)

	p := h.getOrCreatePath(pathName)

	if p.hasPublisher() {
		log.Printf("⚠️ Overriding existing publisher on /%s", pathName)
		p.mu.Lock()
		if p.stream != nil {
			p.stream.Close()
		}
		p.publisher = nil
		p.stream = nil
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}

	stream := gortsplib.NewServerStream(h.server, ctx.Description)
	p.setPublisher(ctx.Session, stream)

	h.sessionsMu.Lock()
	if info, ok := h.sessions[ctx.Session]; ok {
		info.pathName = pathName
		info.isPublisher = true
	}
	h.sessionsMu.Unlock()

	log.Printf("✅ RTSP publisher registered: /%s", pathName)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	pathName := strings.TrimPrefix(ctx.Path, "/")
	p := h.getPath(pathName)
	if p == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	stream := p.getStream()
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, stream, nil
}

func (h *rtspHandler) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	pathName := strings.TrimPrefix(ctx.Path, "/")
	if p := h.getPath(pathName); p != nil {
		p.addReader()
	}
	h.sessionsMu.Lock()
	if info, ok := h.sessions[ctx.Session]; ok {
		info.pathName = pathName
		info.isPublisher = false
	}
	h.sessionsMu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (h *rtspHandler) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	pathName := strings.TrimPrefix(ctx.Path, "/")
	log.Printf("⏺️ RTSP RECORD /%s", pathName)

	p := h.getPath(pathName)
	if p == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}

	stream := p.getStream()
	if stream == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}

	ctx.Session.OnPacketRTPAny(func(medi *description.Media, forma format.Format, pkt *rtp.Packet) {
		stream.WritePacketRTP(medi, pkt)
	})

	log.Printf("✅ RTSP packet relay started: /%s", pathName)
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// GStreamerStream with HEVC support
type GStreamerStream struct {
	Name       string     `json:"name"`
	RTSPUrl    string     `json:"rtsp_url"`
	SRTUrl     string     `json:"srt_url,omitempty"`
	SourceURL  string     `json:"source_url"`
	SourceType StreamType `json:"source_type"`
	Codec      VideoCodec `json:"codec"` // NEW: Video codec (h264, hevc, auto)
	Status     string     `json:"status"`
	PID        int        `json:"pid"`
	Restarts   int        `json:"restarts"`
	LastError  string     `json:"last_error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`

	cmd      *exec.Cmd
	mu       sync.Mutex
	stopChan chan struct{}
	config   *Config
	pipeline string
}

// NewGStreamerStream creates a new stream with codec support
func NewGStreamerStream(name, sourceURL string, sourceType StreamType, codec VideoCodec, config *Config) *GStreamerStream {
	if codec == "" || codec == CodecAuto {
		codec = CodecH264 // Default to H.264 for backward compatibility
	}

	stream := &GStreamerStream{
		Name:       name,
		RTSPUrl:    fmt.Sprintf("rtsp://%s:%d/%s", config.Host, config.RTSPPort, name),
		SourceURL:  sourceURL,
		SourceType: sourceType,
		Codec:      codec,
		Status:     "initializing",
		stopChan:   make(chan struct{}),
		config:     config,
		StartedAt:  time.Now(),
	}
	stream.SRTUrl = fmt.Sprintf("srt://%s:%d?streamid=%s&mode=caller", config.Host, config.SRTPort, name)
	return stream
}

// BuildPipeline builds GStreamer pipeline with codec-specific elements
func (g *GStreamerStream) BuildPipeline() string {
	outputURL := fmt.Sprintf("rtsp://127.0.0.1:%d/%s", g.config.RTSPPort, g.Name)

	// Determine codec-specific GStreamer elements
	var depay, parse string
	switch g.Codec {
	case CodecHEVC:
		depay = "rtph265depay"
		parse = "h265parse config-interval=-1"
	case CodecH264:
		fallthrough
	default:
		depay = "rtph264depay"
		parse = "h264parse config-interval=-1"
	}

	var pipeline string

	switch g.SourceType {
	case StreamTypeSRT, StreamTypeSRTCall:
		// SRT source - demux TS and parse based on codec
		pipeline = fmt.Sprintf(
			`srtsrc uri="%s" latency=%d ! tsdemux ! %s ! rtspclientsink location="%s" protocols=tcp latency=%d`,
			g.SourceURL, g.config.SRTLatency, parse, outputURL, g.config.GstLatency)
	case StreamTypeRTSP:
		fallthrough
	default:
		// RTSP source - depay and parse based on codec
		pipeline = fmt.Sprintf(
			`rtspsrc location="%s" latency=%d buffer-mode=%d drop-on-latency=%t do-retransmission=false protocols=tcp ntp-sync=false ! %s ! %s ! rtspclientsink location="%s" protocols=tcp latency=%d`,
			g.SourceURL, g.config.GstLatency, g.config.GstBufferMode, g.config.GstDropOnLatency, depay, parse, outputURL, g.config.GstLatency)
	}

	g.pipeline = strings.Join(strings.Fields(pipeline), " ")
	return g.pipeline
}

func (g *GStreamerStream) StartWithRetry() {
	retryDelay := 1 * time.Second
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-g.stopChan:
			return
		default:
		}

		err := g.startGStreamer()

		select {
		case <-g.stopChan:
			return
		default:
			g.mu.Lock()
			g.Restarts++
			atomic.AddInt64(&globalMetrics.StreamRestarts, 1)
			if err != nil {
				g.LastError = err.Error()
				atomic.AddInt64(&globalMetrics.StreamErrors, 1)
			}
			g.mu.Unlock()

			log.Printf("🔄 Stream %s: restarting in %v (restart #%d)", g.Name, retryDelay, g.Restarts)
			time.Sleep(retryDelay)

			retryDelay = retryDelay * 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}
	}
}

func (g *GStreamerStream) startGStreamer() error {
	g.mu.Lock()
	pipeline := g.BuildPipeline()

	log.Printf("🎬 GStreamer [%s] (%s, codec: %s):", g.Name, g.SourceType, g.Codec)
	log.Printf("   Source: %s", g.SourceURL)
	log.Printf("   RTSP Output: rtsp://127.0.0.1:%d/%s", g.config.RTSPPort, g.Name)

	g.cmd = exec.Command("bash", "-c", "gst-launch-1.0 -e "+pipeline)
	g.cmd.Stdout = os.Stdout
	g.cmd.Stderr = os.Stderr
	g.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := g.cmd.Start(); err != nil {
		g.Status = "error"
		g.LastError = err.Error()
		g.mu.Unlock()
		return fmt.Errorf("gstreamer start failed: %v", err)
	}

	g.PID = g.cmd.Process.Pid
	g.Status = "running"
	g.LastError = ""
	atomic.AddInt64(&globalMetrics.ActiveStreams, 1)
	log.Printf("✅ GStreamer started [%s] (PID: %d, Codec: %s)", g.Name, g.PID, g.Codec)
	g.mu.Unlock()

	err := g.cmd.Wait()

	g.mu.Lock()
	g.Status = "stopped"
	g.PID = 0
	atomic.AddInt64(&globalMetrics.ActiveStreams, -1)
	if err != nil {
		g.LastError = err.Error()
	}
	g.mu.Unlock()

	return err
}

func (g *GStreamerStream) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()

	select {
	case <-g.stopChan:
	default:
		close(g.stopChan)
	}

	if g.cmd != nil && g.cmd.Process != nil {
		log.Printf("🛑 Stopping GStreamer [%s] (PID: %d)", g.Name, g.PID)
		syscall.Kill(-g.cmd.Process.Pid, syscall.SIGTERM)

		done := make(chan error, 1)
		go func() { done <- g.cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			syscall.Kill(-g.cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	g.Status = "stopped"
}

func (g *GStreamerStream) GetStatus() map[string]interface{} {
	g.mu.Lock()
	defer g.mu.Unlock()

	return map[string]interface{}{
		"name":        g.Name,
		"rtsp_url":    g.RTSPUrl,
		"srt_url":     g.SRTUrl,
		"source_url":  g.SourceURL,
		"source_type": g.SourceType,
		"codec":       g.Codec, // NEW: Include codec in status
		"status":      g.Status,
		"pid":         g.PID,
		"restarts":    g.Restarts,
		"last_error":  g.LastError,
		"started_at":  g.StartedAt.Format(time.RFC3339),
		"uptime":      time.Since(g.StartedAt).String(),
	}
}

// SRTListenerStream with HEVC support
type SRTListenerStream struct {
	Name      string     `json:"name"`
	RTSPUrl   string     `json:"rtsp_url"`
	SRTUrl    string     `json:"srt_url"`
	Codec     VideoCodec `json:"codec"` // NEW: Expected video codec
	Status    string     `json:"status"`
	PID       int        `json:"pid"`
	Restarts  int        `json:"restarts"`
	LastError string     `json:"last_error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	Port      int        `json:"port"`

	cmd      *exec.Cmd
	mu       sync.Mutex
	stopChan chan struct{}
	config   *Config
}

// NewSRTListenerStream creates a new SRT listener with codec support
func NewSRTListenerStream(name string, port int, codec VideoCodec, config *Config) *SRTListenerStream {
	if codec == "" || codec == CodecAuto {
		codec = CodecHEVC // Default to HEVC for SRT listeners (modern cameras)
	}

	return &SRTListenerStream{
		Name:      name,
		RTSPUrl:   fmt.Sprintf("rtsp://%s:%d/%s", config.Host, config.RTSPPort, name),
		SRTUrl:    fmt.Sprintf("srt://%s:%d?streamid=%s&mode=listener", config.Host, port, name),
		Codec:     codec,
		Status:    "initializing",
		Port:      port,
		stopChan:  make(chan struct{}),
		config:    config,
		StartedAt: time.Now(),
	}
}

// BuildPipeline builds GStreamer pipeline with codec-specific parser
func (s *SRTListenerStream) BuildPipeline() string {
	outputURL := fmt.Sprintf("rtsp://127.0.0.1:%d/%s", s.config.RTSPPort, s.Name)

	// Determine codec-specific parser
	var parse string
	switch s.Codec {
	case CodecHEVC:
		parse = "h265parse config-interval=-1"
	case CodecH264:
		fallthrough
	default:
		parse = "h264parse config-interval=-1"
	}

	var srtParams string
	if s.config.SRTPassphrase != "" {
		srtParams = fmt.Sprintf("mode=listener&latency=%d&passphrase=%s", s.config.SRTLatency, s.config.SRTPassphrase)
	} else {
		srtParams = fmt.Sprintf("mode=listener&latency=%d", s.config.SRTLatency)
	}

	return fmt.Sprintf(
		`srtsrc uri="srt://0.0.0.0:%d?%s" ! tsdemux ! %s ! rtspclientsink location="%s" protocols=tcp latency=0`,
		s.Port, srtParams, parse, outputURL)
}

func (s *SRTListenerStream) StartWithRetry() {
	retryDelay := 1 * time.Second
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-s.stopChan:
			return
		default:
		}

		err := s.startGStreamer()

		select {
		case <-s.stopChan:
			return
		default:
			s.mu.Lock()
			s.Restarts++
			atomic.AddInt64(&globalMetrics.StreamRestarts, 1)
			if err != nil {
				s.LastError = err.Error()
				atomic.AddInt64(&globalMetrics.StreamErrors, 1)
			}
			s.mu.Unlock()

			log.Printf("🔄 SRT Listener [%s]: restarting in %v", s.Name, retryDelay)
			time.Sleep(retryDelay)

			retryDelay = retryDelay * 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		}
	}
}

func (s *SRTListenerStream) startGStreamer() error {
	s.mu.Lock()
	pipeline := s.BuildPipeline()

	log.Printf("🎧 SRT Listener [%s] (codec: %s):", s.Name, s.Codec)
	log.Printf("   SRT: srt://0.0.0.0:%d (waiting for connection)", s.Port)
	log.Printf("   RTSP Output: rtsp://127.0.0.1:%d/%s", s.config.RTSPPort, s.Name)

	s.cmd = exec.Command("bash", "-c", "gst-launch-1.0 -e "+pipeline)
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := s.cmd.Start(); err != nil {
		s.Status = "error"
		s.LastError = err.Error()
		s.mu.Unlock()
		return fmt.Errorf("gstreamer start failed: %v", err)
	}

	s.PID = s.cmd.Process.Pid
	s.Status = "listening"
	s.LastError = ""
	atomic.AddInt64(&globalMetrics.ActiveStreams, 1)
	log.Printf("✅ SRT Listener started [%s] (PID: %d, Port: %d, Codec: %s)", s.Name, s.PID, s.Port, s.Codec)
	s.mu.Unlock()

	err := s.cmd.Wait()

	s.mu.Lock()
	s.Status = "stopped"
	s.PID = 0
	atomic.AddInt64(&globalMetrics.ActiveStreams, -1)
	if err != nil {
		s.LastError = err.Error()
	}
	s.mu.Unlock()

	return err
}

func (s *SRTListenerStream) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.stopChan:
	default:
		close(s.stopChan)
	}

	if s.cmd != nil && s.cmd.Process != nil {
		log.Printf("🛑 Stopping SRT Listener [%s]", s.Name)
		syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)

		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}

	s.Status = "stopped"
}

func (s *SRTListenerStream) GetStatus() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return map[string]interface{}{
		"name":       s.Name,
		"rtsp_url":   s.RTSPUrl,
		"srt_url":    s.SRTUrl,
		"codec":      s.Codec, // NEW: Include codec in status
		"port":       s.Port,
		"status":     s.Status,
		"pid":        s.PID,
		"restarts":   s.Restarts,
		"last_error": s.LastError,
		"started_at": s.StartedAt.Format(time.RFC3339),
	}
}

type GStreamerModule struct {
	config       *Config
	rtspHandler  *rtspHandler
	rtspServer   *gortsplib.Server
	streams      map[string]*GStreamerStream
	srtListeners map[string]*SRTListenerStream
	nextSRTPort  int
	mu           sync.RWMutex
	ctx          context.Context
	cancel       context.CancelFunc
}

func NewGStreamerModule(config *Config) *GStreamerModule {
	ctx, cancel := context.WithCancel(context.Background())
	handler := newRTSPHandler()

	udpRTPPort := ((config.RTSPPort / 2) * 2) + 2
	udpRTCPPort := udpRTPPort + 1

	server := &gortsplib.Server{
		Handler:           handler,
		RTSPAddress:       fmt.Sprintf(":%d", config.RTSPPort),
		UDPRTPAddress:     fmt.Sprintf(":%d", udpRTPPort),
		UDPRTCPAddress:    fmt.Sprintf(":%d", udpRTCPPort),
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8600,
		MulticastRTCPPort: 8601,
	}

	handler.server = server

	return &GStreamerModule{
		config:       config,
		rtspHandler:  handler,
		rtspServer:   server,
		streams:      make(map[string]*GStreamerStream),
		srtListeners: make(map[string]*SRTListenerStream),
		nextSRTPort:  config.SRTPort,
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (m *GStreamerModule) Start() error {
	if !m.config.GStreamerEnabled {
		log.Println("🎬 GStreamer module disabled")
		return nil
	}

	if err := m.rtspServer.Start(); err != nil {
		return fmt.Errorf("failed to start RTSP server: %v", err)
	}

	if err := m.RestoreStreams(); err != nil {
		log.Printf("⚠️ Failed to restore streams: %v", err)
	}

	log.Printf("🎬 GStreamer module started (RTSP: %d, SRT: %d) - HEVC Support Enabled", m.config.RTSPPort, m.config.SRTPort)
	return nil
}

func (m *GStreamerModule) Stop() {
	m.cancel()

	m.mu.Lock()
	for _, stream := range m.streams {
		stream.Stop()
	}
	for _, listener := range m.srtListeners {
		listener.Stop()
	}
	m.mu.Unlock()

	m.rtspServer.Close()
	log.Println("🎬 GStreamer module stopped")
}

func (m *GStreamerModule) getStateFilePath() string {
	if path := os.Getenv("STATE_FILE"); path != "" {
		return path
	}
	if m.config.StreamStateFile != "" {
		return m.config.StreamStateFile
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "streams_state.json")
	}
	return "streams_state.json"
}

func (m *GStreamerModule) SaveState() error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	state := &PersistentState{
		Streams:   make([]StreamState, 0),
		UpdatedAt: time.Now().Format(time.RFC3339),
	}

	for _, stream := range m.streams {
		stream.mu.Lock()
		state.Streams = append(state.Streams, StreamState{
			Name:       stream.Name,
			SourceURL:  stream.SourceURL,
			StreamType: stream.SourceType,
			Codec:      string(stream.Codec), // NEW: Save codec
			CreatedAt:  stream.StartedAt.Format(time.RFC3339),
		})
		stream.mu.Unlock()
	}

	for _, listener := range m.srtListeners {
		listener.mu.Lock()
		state.Streams = append(state.Streams, StreamState{
			Name:       listener.Name,
			StreamType: StreamTypeSRT,
			Codec:      string(listener.Codec), // NEW: Save codec
			Port:       listener.Port,
			CreatedAt:  listener.StartedAt.Format(time.RFC3339),
		})
		listener.mu.Unlock()
	}

	stateFile := m.getStateFilePath()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := stateFile + ".tmp"
	if err := ioutil.WriteFile(tmpFile, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, stateFile)
}

func (m *GStreamerModule) RestoreStreams() error {
	stateFile := m.getStateFilePath()
	data, err := ioutil.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	if len(state.Streams) == 0 {
		return nil
	}

	log.Printf("🔄 Restoring %d streams...", len(state.Streams))

	for _, ss := range state.Streams {
		// Normalize codec (handle backward compatibility)
		codec := normalizeCodec(ss.Codec)

		if ss.Port > 0 && ss.SourceURL == "" {
			log.Printf("   ↳ Restoring SRT Listener: %s (port %d, codec: %s)", ss.Name, ss.Port, codec)
			m.AddSRTListenerWithPort(ss.Name, ss.Port, codec, false)
		} else {
			log.Printf("   ↳ Restoring: %s (%s, codec: %s)", ss.Name, ss.StreamType, codec)
			m.AddStream(ss.Name, ss.SourceURL, ss.StreamType, codec, false)
		}
		time.Sleep(500 * time.Millisecond)
	}

	return nil
}

// AddStream creates a new stream with codec support
func (m *GStreamerModule) AddStream(name, sourceURL string, sourceType StreamType, codec VideoCodec, persist bool) (*GStreamerStream, error) {
	m.mu.Lock()

	if existing, ok := m.streams[name]; ok {
		m.mu.Unlock()
		existing.Stop()
		m.mu.Lock()
		delete(m.streams, name)
		time.Sleep(300 * time.Millisecond)
	}

	stream := NewGStreamerStream(name, sourceURL, sourceType, codec, m.config)
	m.streams[name] = stream
	atomic.AddInt64(&globalMetrics.TotalStreams, 1)
	m.mu.Unlock()

	go stream.StartWithRetry()

	if persist {
		m.SaveState()
	}

	return stream, nil
}

// AddSRTListener creates a new SRT listener with default codec (HEVC)
func (m *GStreamerModule) AddSRTListener(name string, codec VideoCodec, persist bool) (*SRTListenerStream, error) {
	m.mu.Lock()
	port := m.nextSRTPort
	m.nextSRTPort++
	m.mu.Unlock()

	return m.AddSRTListenerWithPort(name, port, codec, persist)
}

// AddSRTListenerWithPort creates a new SRT listener with specific port and codec
func (m *GStreamerModule) AddSRTListenerWithPort(name string, port int, codec VideoCodec, persist bool) (*SRTListenerStream, error) {
	m.mu.Lock()

	if existing, ok := m.srtListeners[name]; ok {
		m.mu.Unlock()
		existing.Stop()
		m.mu.Lock()
		delete(m.srtListeners, name)
		time.Sleep(300 * time.Millisecond)
	}

	if port >= m.nextSRTPort {
		m.nextSRTPort = port + 1
	}

	listener := NewSRTListenerStream(name, port, codec, m.config)
	m.srtListeners[name] = listener
	atomic.AddInt64(&globalMetrics.TotalStreams, 1)
	m.mu.Unlock()

	go listener.StartWithRetry()

	if persist {
		m.SaveState()
	}

	return listener, nil
}

func (m *GStreamerModule) RemoveStream(name string) error {
	m.mu.Lock()

	if stream, ok := m.streams[name]; ok {
		delete(m.streams, name)
		m.mu.Unlock()
		stream.Stop()
		m.rtspHandler.removePath(name)
		m.SaveState()
		return nil
	}

	if listener, ok := m.srtListeners[name]; ok {
		delete(m.srtListeners, name)
		m.mu.Unlock()
		listener.Stop()
		m.rtspHandler.removePath(name)
		m.SaveState()
		return nil
	}

	m.mu.Unlock()
	return fmt.Errorf("stream not found: %s", name)
}

func (m *GStreamerModule) ListStreams() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	list := make([]map[string]interface{}, 0)

	for _, stream := range m.streams {
		list = append(list, stream.GetStatus())
	}

	for _, listener := range m.srtListeners {
		list = append(list, listener.GetStatus())
	}

	return list
}

func (m *GStreamerModule) GetStreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams) + len(m.srtListeners)
}

// =============================================================================
// UNIFIED DEVICES SERVER
// =============================================================================

type UnifiedServer struct {
	config       *Config
	lidarModule  *LidarModule
	radarModule  *RadarModule
	gstreamerMod *GStreamerModule
	rateLimiter  *RateLimiterPool
	connTracker  *ConnectionTracker
	httpServer   *http.Server
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

func NewUnifiedServer(config *Config) *UnifiedServer {
	ctx, cancel := context.WithCancel(context.Background())

	server := &UnifiedServer{
		config:       config,
		lidarModule:  NewLidarModule(config),
		radarModule:  NewRadarModule(config),
		gstreamerMod: NewGStreamerModule(config),
		rateLimiter:  NewRateLimiterPool(config.RateLimitRPS, config.RateLimitBurst),
		connTracker:  NewConnectionTracker(config.MaxConnections),
		ctx:          ctx,
		cancel:       cancel,
	}

	return server
}

func (s *UnifiedServer) Start() error {
	// Start modules
	if err := s.lidarModule.Start(); err != nil {
		return fmt.Errorf("lidar module start failed: %v", err)
	}

	if err := s.radarModule.Start(); err != nil {
		return fmt.Errorf("radar module start failed: %v", err)
	}

	if err := s.gstreamerMod.Start(); err != nil {
		return fmt.Errorf("gstreamer module start failed: %v", err)
	}

	// Setup HTTP routes
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.config.APIPort),
		Handler:      s.middleware(mux),
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
		IdleTimeout:  s.config.IdleTimeout,
	}

	// Start HTTP server
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		log.Printf("🚀 HTTP API started on port %d", s.config.APIPort)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()

	// Start health checker
	s.wg.Add(1)
	go s.healthChecker()

	globalMetrics.HealthStatus = "healthy"
	return nil
}

func (s *UnifiedServer) Stop() {
	log.Println("🛑 Shutting down server...")

	s.cancel()

	// Graceful HTTP shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
	defer shutdownCancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("⚠️ HTTP shutdown error: %v", err)
	}

	// Stop modules
	s.gstreamerMod.Stop()
	s.radarModule.Stop()
	s.lidarModule.Stop()

	s.wg.Wait()
	log.Println("✅ Server stopped gracefully")
}

func (s *UnifiedServer) healthChecker() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			globalMetrics.mu.Lock()
			globalMetrics.LastHealthCheck = time.Now()
			globalMetrics.HealthStatus = "healthy"
			globalMetrics.mu.Unlock()
		}
	}
}

func (s *UnifiedServer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS
		if s.config.EnableCORS {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
		}

		// API Key authentication
		if s.config.APIKey != "" {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey == "" {
				apiKey = r.URL.Query().Get("api_key")
			}
			// Allow health and metrics endpoints without auth
			if !strings.HasPrefix(r.URL.Path, "/health") && !strings.HasPrefix(r.URL.Path, "/metrics") {
				if apiKey != s.config.APIKey {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
			}
		}

		// Rate limiting
		if s.config.RateLimitEnabled {
			ip := getClientIP(r)
			limiter := s.rateLimiter.GetLimiter(ip)
			if !limiter.Allow() {
				atomic.AddInt64(&globalMetrics.RejectedConnections, 1)
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
		}

		// Connection tracking
		connID := fmt.Sprintf("%s-%d", getClientIP(r), time.Now().UnixNano())
		if !s.connTracker.Add(connID) {
			atomic.AddInt64(&globalMetrics.RejectedConnections, 1)
			http.Error(w, `{"error":"max connections exceeded"}`, http.StatusServiceUnavailable)
			return
		}
		defer s.connTracker.Remove(connID)

		next.ServeHTTP(w, r)
	})
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.Split(xff, ",")[0]
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}

func (s *UnifiedServer) registerRoutes(mux *http.ServeMux) {
	// Health & Metrics
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/", s.handleIndex)

	// LiDAR endpoints
	mux.HandleFunc("/lidar/points", s.handleLidarPoints)
	mux.HandleFunc("/lidar/stats", s.handleLidarStats)
	mux.HandleFunc("/lidar/devices", s.handleLidarDevices)
	mux.HandleFunc("/lidar/clear", s.handleLidarClear)
	mux.HandleFunc("/ws/lidar/ingest", s.lidarModule.HandleWebSocketIngest)

	// Radar endpoints
	mux.HandleFunc("/radar/targets", s.handleRadarTargets)
	mux.HandleFunc("/radar/stats", s.handleRadarStats)
	mux.HandleFunc("/radar/devices", s.handleRadarDevices)
	mux.HandleFunc("/radar/frame", s.handleRadarFrame)
	mux.HandleFunc("/radar/clear", s.handleRadarClear)
	mux.HandleFunc("/ws/radar/ingest", s.radarModule.HandleWebSocketIngest)

	// GStreamer endpoints
	mux.HandleFunc("/gstreamer/streams", s.handleGStreamerStreams)
	mux.HandleFunc("/gstreamer/streams/", s.handleGStreamerStreamByName)

	// Legacy compatibility endpoints
	mux.HandleFunc("/api/points", s.handleLidarPoints)
	mux.HandleFunc("/api/targets", s.handleRadarTargets)
	mux.HandleFunc("/api/streams", s.handleGStreamerStreams)
}

// =============================================================================
// HTTP HANDLERS - HEALTH & METRICS
// =============================================================================

func (s *UnifiedServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	lidarLive := false
	radarLive := false
	streamsActive := 0

	if s.config.LidarEnabled {
		s.lidarModule.defaultCloud.mu.RLock()
		lidarLive = time.Since(s.lidarModule.defaultCloud.lastUpdate) < 5*time.Second
		s.lidarModule.defaultCloud.mu.RUnlock()
	}

	if s.config.RadarEnabled {
		s.radarModule.defaultData.mu.RLock()
		radarLive = time.Since(s.radarModule.defaultData.LastUpdate) < 5*time.Second
		s.radarModule.defaultData.mu.RUnlock()
	}

	if s.config.GStreamerEnabled {
		streamsActive = s.gstreamerMod.GetStreamCount()
	}

	health := map[string]interface{}{
		"status":  globalMetrics.HealthStatus,
		"version": "2.1.0-hevc",
		"uptime":  time.Since(globalMetrics.StartTime).String(),
		"modules": map[string]interface{}{
			"lidar": map[string]interface{}{
				"enabled":      s.config.LidarEnabled,
				"is_live":      lidarLive,
				"device_count": atomic.LoadInt64(&globalMetrics.LidarDevices),
			},
			"radar": map[string]interface{}{
				"enabled":      s.config.RadarEnabled,
				"is_live":      radarLive,
				"device_count": atomic.LoadInt64(&globalMetrics.RadarDevices),
			},
			"gstreamer": map[string]interface{}{
				"enabled":        s.config.GStreamerEnabled,
				"active_streams": streamsActive,
				"rtsp_port":      s.config.RTSPPort,
				"srt_port":       s.config.SRTPort,
				"hevc_support":   true, // NEW: Indicate HEVC support
			},
		},
		"connections": map[string]interface{}{
			"active": atomic.LoadInt64(&globalMetrics.ActiveConnections),
			"max":    s.config.MaxConnections,
		},
	}

	json.NewEncoder(w).Encode(health)
}

func (s *UnifiedServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(globalMetrics.GetSnapshot())
}

func (s *UnifiedServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	tmpl := template.Must(template.New("index").Parse(indexHTML))
	tmpl.Execute(w, map[string]interface{}{
		"APIPort":  s.config.APIPort,
		"RTSPPort": s.config.RTSPPort,
		"SRTPort":  s.config.SRTPort,
	})
}

// =============================================================================
// HTTP HANDLERS - LIDAR
// =============================================================================

func (s *UnifiedServer) handleLidarPoints(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	deviceID := r.URL.Query().Get("device")
	cloud := s.lidarModule.GetCloud(deviceID)

	maxPoints := s.config.LidarMaxPoints
	if maxStr := r.URL.Query().Get("max"); maxStr != "" {
		if m, err := strconv.Atoi(maxStr); err == nil && m > 0 {
			maxPoints = m
		}
	}

	cloud.mu.RLock()
	points := cloud.Points
	if len(points) > maxPoints {
		points = points[len(points)-maxPoints:]
	}
	bounds := map[string]float64{
		"minX": cloud.minX, "maxX": cloud.maxX,
		"minY": cloud.minY, "maxY": cloud.maxY,
		"minZ": cloud.minZ, "maxZ": cloud.maxZ,
	}
	cloud.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"points": points,
		"bounds": bounds,
		"count":  len(points),
	})
}

func (s *UnifiedServer) handleLidarStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	deviceID := r.URL.Query().Get("device")
	cloud := s.lidarModule.GetCloud(deviceID)

	json.NewEncoder(w).Encode(cloud.GetStats())
}

func (s *UnifiedServer) handleLidarDevices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	devices := s.lidarModule.GetDevices()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": devices,
		"count":   len(devices),
	})
}

func (s *UnifiedServer) handleLidarClear(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	deviceID := r.URL.Query().Get("device")
	s.lidarModule.ClearCloud(deviceID)

	json.NewEncoder(w).Encode(map[string]string{
		"status":  "cleared",
		"message": "LiDAR data cleared",
	})
}

// =============================================================================
// HTTP HANDLERS - RADAR
// =============================================================================

func (s *UnifiedServer) handleRadarTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	deviceID := r.URL.Query().Get("device")
	device := s.radarModule.GetDevice(deviceID)

	maxTargets := s.config.RadarMaxTargets
	if maxStr := r.URL.Query().Get("max"); maxStr != "" {
		if m, err := strconv.Atoi(maxStr); err == nil && m > 0 {
			maxTargets = m
		}
	}

	device.mu.RLock()
	targets := device.Targets
	if len(targets) > maxTargets {
		targets = targets[len(targets)-maxTargets:]
	}
	device.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"device_id":    device.DeviceID,
		"targets":      targets,
		"target_count": len(targets),
		"timestamp":    time.Now().Format(time.RFC3339),
	})
}

func (s *UnifiedServer) handleRadarStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	deviceID := r.URL.Query().Get("device")
	json.NewEncoder(w).Encode(s.radarModule.GetStats(deviceID))
}

func (s *UnifiedServer) handleRadarDevices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	devices := s.radarModule.GetDevices()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"devices": devices,
		"count":   len(devices),
	})
}

func (s *UnifiedServer) handleRadarFrame(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	deviceID := r.URL.Query().Get("device")
	device := s.radarModule.GetDevice(deviceID)

	device.mu.RLock()
	frame := device.LastFrame
	device.mu.RUnlock()

	if frame == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"device_id": device.DeviceID,
			"frame":     nil,
			"message":   "No frames received yet",
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"device_id": device.DeviceID,
		"frame":     frame,
	})
}

func (s *UnifiedServer) handleRadarClear(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	deviceID := r.URL.Query().Get("device")
	s.radarModule.ClearData(deviceID)

	json.NewEncoder(w).Encode(map[string]string{
		"status":  "cleared",
		"message": "Radar data cleared",
	})
}

// =============================================================================
// HTTP HANDLERS - GSTREAMER (WITH CODEC SUPPORT)
// =============================================================================

func (s *UnifiedServer) handleGStreamerStreams(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"streams":      s.gstreamerMod.ListStreams(),
			"hevc_support": true, // NEW: Indicate HEVC support in API response
		})

	case "POST":
		var req struct {
			Name       string `json:"name"`
			SourceURL  string `json:"source_url"`
			SourceType string `json:"source_type"`
			Codec      string `json:"codec"` // NEW: Accept codec parameter
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "invalid json"})
			return
		}

		if req.Name == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "name required"})
			return
		}

		// Normalize codec
		codec := normalizeCodec(req.Codec)

		if req.SourceType == "srt_listener" {
			// For SRT listeners, default to HEVC if no codec specified (modern cameras)
			if codec == CodecAuto {
				codec = CodecHEVC
			}
			listener, err := s.gstreamerMod.AddSRTListener(req.Name, codec, true)
			if err != nil {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
				return
			}
			time.Sleep(1 * time.Second)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"stream":  listener.GetStatus(),
				"message": fmt.Sprintf("Push SRT to: srt://%s:%d (codec: %s)", s.config.Host, listener.Port, codec),
			})
			return
		}

		if req.SourceURL == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "source_url required"})
			return
		}

		sourceType := StreamTypeRTSP
		if req.SourceType == "srt" {
			sourceType = StreamTypeSRT
		}

		// For RTSP sources, default to H.264 if no codec specified (backward compatibility)
		if codec == CodecAuto {
			codec = CodecH264
		}

		stream, err := s.gstreamerMod.AddStream(req.Name, req.SourceURL, sourceType, codec, true)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			return
		}

		time.Sleep(1 * time.Second)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"stream":  stream.GetStatus(),
		})
	}
}

func (s *UnifiedServer) handleGStreamerStreamByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/gstreamer/streams/")
	w.Header().Set("Content-Type", "application/json")

	if r.Method == "DELETE" {
		if err := s.gstreamerMod.RemoveStream(name); err != nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "deleted"})
	}
}

// =============================================================================
// HTML TEMPLATE
// =============================================================================

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Unified Devices Server - HEVC Enabled</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: 'Segoe UI', system-ui, sans-serif;
            background: linear-gradient(135deg, #0d1117 0%, #161b22 50%, #21262d 100%);
            color: #e6edf3;
            min-height: 100vh;
            padding: 40px;
        }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { color: #58a6ff; margin-bottom: 10px; font-size: 2.5em; }
        .subtitle { color: #8b949e; margin-bottom: 40px; }
        .hevc-badge { background: #238636; padding: 4px 12px; border-radius: 12px; font-size: 12px; margin-left: 10px; }
        .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(350px, 1fr)); gap: 20px; }
        .card {
            background: rgba(255,255,255,0.05);
            border-radius: 12px;
            padding: 25px;
            border: 1px solid rgba(255,255,255,0.1);
        }
        .card h2 {
            display: flex;
            align-items: center;
            gap: 10px;
            margin-bottom: 15px;
            color: #58a6ff;
        }
        .card h2 .icon { font-size: 1.5em; }
        .endpoint {
            background: rgba(0,0,0,0.3);
            padding: 10px 15px;
            border-radius: 6px;
            margin: 8px 0;
            font-family: monospace;
            font-size: 13px;
        }
        .endpoint .method { color: #7ee787; font-weight: bold; }
        .endpoint .path { color: #79c0ff; }
        .status { margin-top: 30px; }
        .status-item {
            display: flex;
            justify-content: space-between;
            padding: 10px 0;
            border-bottom: 1px solid rgba(255,255,255,0.1);
        }
        .status-value { color: #7ee787; font-weight: bold; }
        .badge {
            display: inline-block;
            padding: 2px 8px;
            border-radius: 10px;
            font-size: 11px;
            font-weight: bold;
        }
        .badge.live { background: rgba(0,255,0,0.2); color: #7ee787; }
        .badge.offline { background: rgba(255,0,0,0.2); color: #ff7b72; }
        .codec-info { background: rgba(35,134,54,0.2); padding: 15px; border-radius: 8px; margin-top: 15px; }
        .codec-info h3 { color: #7ee787; margin-bottom: 10px; font-size: 14px; }
        .codec-info code { background: rgba(0,0,0,0.3); padding: 2px 6px; border-radius: 4px; }
    </style>
</head>
<body>
    <div class="container">
        <h1>🚀 Unified Devices Server <span class="hevc-badge">HEVC Enabled</span></h1>
        <p class="subtitle">LiDAR + Radar + GStreamer - Combined Edge Device Management with H.264/H.265 Support</p>

        <div class="grid">
            <div class="card">
                <h2><span class="icon">📡</span> LiDAR Module</h2>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/lidar/points</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/lidar/stats</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/lidar/devices</span></div>
                <div class="endpoint"><span class="method">POST</span> <span class="path">/lidar/clear</span></div>
                <div class="endpoint"><span class="method">WS</span> <span class="path">/ws/lidar/ingest</span></div>
            </div>

            <div class="card">
                <h2><span class="icon">🎯</span> Radar Module</h2>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/radar/targets</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/radar/stats</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/radar/devices</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/radar/frame</span></div>
                <div class="endpoint"><span class="method">WS</span> <span class="path">/ws/radar/ingest</span></div>
            </div>

            <div class="card">
                <h2><span class="icon">🎬</span> GStreamer Module</h2>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/gstreamer/streams</span></div>
                <div class="endpoint"><span class="method">POST</span> <span class="path">/gstreamer/streams</span></div>
                <div class="endpoint"><span class="method">DELETE</span> <span class="path">/gstreamer/streams/{name}</span></div>
                <p style="margin-top:15px;color:#8b949e;font-size:13px;">
                    RTSP: rtsp://host:{{.RTSPPort}}/{stream}<br>
                    SRT Base: srt://host:{{.SRTPort}}
                </p>
                <div class="codec-info">
                    <h3>📦 Codec Support</h3>
                    <p style="font-size:12px;color:#8b949e;">
                        Create streams with codec parameter:<br><br>
                        <code>"codec": "h264"</code> - H.264/AVC streams<br>
                        <code>"codec": "hevc"</code> - H.265/HEVC streams<br>
                        <code>"codec": "auto"</code> - Auto-detect (default)<br><br>
                        SRT Listeners default to HEVC for modern cameras.
                    </p>
                </div>
            </div>

            <div class="card">
                <h2><span class="icon">📊</span> Monitoring</h2>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/health</span></div>
                <div class="endpoint"><span class="method">GET</span> <span class="path">/metrics</span></div>
                <div class="status" id="status">Loading...</div>
            </div>
        </div>
    </div>

    <script>
    async function updateStatus() {
        try {
            const res = await fetch('/health');
            const data = await res.json();
            document.getElementById('status').innerHTML = 
                '<div class="status-item"><span>Status</span><span class="status-value">' + data.status + '</span></div>' +
                '<div class="status-item"><span>Version</span><span class="status-value">' + data.version + '</span></div>' +
                '<div class="status-item"><span>Uptime</span><span class="status-value">' + data.uptime + '</span></div>' +
                '<div class="status-item"><span>Connections</span><span class="status-value">' + data.connections.active + '/' + data.connections.max + '</span></div>' +
                '<div class="status-item"><span>LiDAR</span><span class="badge ' + (data.modules.lidar.is_live ? 'live' : 'offline') + '">' + (data.modules.lidar.is_live ? 'LIVE' : 'OFFLINE') + '</span></div>' +
                '<div class="status-item"><span>Radar</span><span class="badge ' + (data.modules.radar.is_live ? 'live' : 'offline') + '">' + (data.modules.radar.is_live ? 'LIVE' : 'OFFLINE') + '</span></div>' +
                '<div class="status-item"><span>Streams</span><span class="status-value">' + data.modules.gstreamer.active_streams + '</span></div>' +
                '<div class="status-item"><span>HEVC Support</span><span class="badge live">ENABLED</span></div>';
        } catch(e) {
            document.getElementById('status').innerHTML = '<div class="status-item"><span>Error</span><span class="badge offline">UNAVAILABLE</span></div>';
        }
    }
    updateStatus();
    setInterval(updateStatus, 5000);
    </script>
</body>
</html>`

// =============================================================================
// BANNER
// =============================================================================

func printBanner(config *Config) {
	fmt.Printf(`
╔═══════════════════════════════════════════════════════════════════════════════╗
║      🚀 UNIFIED DEVICES SERVER v2.1 - HEVC Support Enabled 🚀                ║
╚═══════════════════════════════════════════════════════════════════════════════╝

  ═══════════════════════════════════════════════════════════════════════════════
  📡 MODULES
  ═══════════════════════════════════════════════════════════════════════════════

  LiDAR:     %s (UDP: %d, Voxel: %.2fcm)
  Radar:     %s (TTL: %.1fs, Range: %.1fm)
  GStreamer: %s (RTSP: %d, SRT: %d) [HEVC: ✅]

  ═══════════════════════════════════════════════════════════════════════════════
  🎬 CODEC SUPPORT
  ═══════════════════════════════════════════════════════════════════════════════

  H.264/AVC:    ✅ Supported (codec: "h264")
  H.265/HEVC:   ✅ Supported (codec: "hevc")
  Auto-detect:  ✅ Supported (codec: "auto")

  SRT Listeners default to HEVC for modern cameras.
  RTSP sources default to H.264 for backward compatibility.

  ═══════════════════════════════════════════════════════════════════════════════
  🌐 ENDPOINTS
  ═══════════════════════════════════════════════════════════════════════════════

  HTTP API:        http://0.0.0.0:%d
  Health:          http://0.0.0.0:%d/health
  Metrics:         http://0.0.0.0:%d/metrics

  LiDAR Points:    http://0.0.0.0:%d/lidar/points
  LiDAR WS:        ws://0.0.0.0:%d/ws/lidar/ingest

  Radar Targets:   http://0.0.0.0:%d/radar/targets
  Radar WS:        ws://0.0.0.0:%d/ws/radar/ingest

  GStreamer API:   http://0.0.0.0:%d/gstreamer/streams
  RTSP Server:     rtsp://0.0.0.0:%d/{stream_name}

  ═══════════════════════════════════════════════════════════════════════════════
  🔒 SECURITY
  ═══════════════════════════════════════════════════════════════════════════════

  Rate Limiting:   %s (%.0f req/s, burst: %d)
  API Key:         %s
  Max Connections: %d

  ═══════════════════════════════════════════════════════════════════════════════

  [Ctrl+C to stop]

`,
		enabledStr(config.LidarEnabled), config.LidarUDPPort, config.LidarVoxelSize*100,
		enabledStr(config.RadarEnabled), config.RadarTTLSeconds, config.RadarMaxRange,
		enabledStr(config.GStreamerEnabled), config.RTSPPort, config.SRTPort,
		config.APIPort, config.APIPort, config.APIPort,
		config.APIPort, config.APIPort,
		config.APIPort, config.APIPort,
		config.APIPort, config.RTSPPort,
		enabledStr(config.RateLimitEnabled), config.RateLimitRPS, config.RateLimitBurst,
		apiKeyStatus(config.APIKey), config.MaxConnections,
	)
}

func enabledStr(enabled bool) string {
	if enabled {
		return "✅ Enabled"
	}
	return "❌ Disabled"
}

func apiKeyStatus(key string) string {
	if key != "" {
		return "✅ Configured"
	}
	return "⚠️ Not set (open access)"
}

// =============================================================================
// MAIN
// =============================================================================

func main() {
	config := DefaultConfig()

	// Parse command line flags
	flag.IntVar(&config.APIPort, "port", config.APIPort, "HTTP API port")
	flag.IntVar(&config.LidarUDPPort, "lidar-udp", config.LidarUDPPort, "LiDAR UDP port")
	flag.IntVar(&config.RTSPPort, "rtsp", config.RTSPPort, "RTSP server port")
	flag.IntVar(&config.SRTPort, "srt", config.SRTPort, "SRT base port")
	flag.BoolVar(&config.LidarEnabled, "lidar", config.LidarEnabled, "Enable LiDAR module")
	flag.BoolVar(&config.RadarEnabled, "radar", config.RadarEnabled, "Enable Radar module")
	flag.BoolVar(&config.GStreamerEnabled, "gstreamer", config.GStreamerEnabled, "Enable GStreamer module")
	flag.Float64Var(&config.LidarVoxelSize, "voxel-size", config.LidarVoxelSize, "LiDAR voxel size in meters")
	flag.Float64Var(&config.RadarTTLSeconds, "radar-ttl", config.RadarTTLSeconds, "Radar target TTL in seconds")
	flag.Float64Var(&config.RadarMaxRange, "radar-range", config.RadarMaxRange, "Radar max range in meters")
	flag.IntVar(&config.SRTLatency, "srt-latency", config.SRTLatency, "SRT latency in ms")
	flag.IntVar(&config.GstLatency, "gst-latency", config.GstLatency, "GStreamer latency in ms")
	flag.StringVar(&config.APIKey, "api-key", config.APIKey, "API key for authentication")
	flag.BoolVar(&config.RateLimitEnabled, "rate-limit", config.RateLimitEnabled, "Enable rate limiting")
	flag.Float64Var(&config.RateLimitRPS, "rate-limit-rps", config.RateLimitRPS, "Rate limit requests per second")
	flag.IntVar(&config.MaxConnections, "max-conn", config.MaxConnections, "Maximum concurrent connections")
	flag.Parse()

	// Load from environment
	LoadConfigFromEnv(config)

	// Print banner
	printBanner(config)

	// Create and start server
	server := NewUnifiedServer(config)

	if err := server.Start(); err != nil {
		log.Fatalf("❌ Failed to start server: %v", err)
	}

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	// Graceful shutdown
	server.Stop()
}