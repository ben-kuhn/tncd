// Package config loads, validates, and migrates tncd INI configuration files.
// It is drop-in compatible with the Python 1.x tncd.ini format.
package config

import (
	"fmt"
	"log"
	"sort"
	"strconv"

	"github.com/ben-kuhn/tncd/v2/kiss"
	"gopkg.in/ini.v1"
)

// Server holds the [server] section settings.
type Server struct {
	ListenHost  string // default "127.0.0.1"
	ListenPort  int    // default 8000
	Callsign    string // default "AGWPE"
	MaxClients  int    // default 8
	IdleTimeout int    // seconds, default 300, <=0 disables
}

// AX25 holds the [ax25] section settings.
type AX25 struct {
	MaxWindow int // default 3, clamped 1..7
	N2Retry   int // default 10
	T3Timeout int // seconds, default 180, <=0 disables
}

// Port holds one [client.N] section's settings plus the associated [kiss.N] params.
type Port struct {
	Name string // default "Port N"
	Type string // "serial" | "tcp" | "bluetooth"

	// serial
	Device         string
	SerialBaudrate int     // default 9600; legacy key "baudrate" honored
	Parity         string  // default "N"
	StopBits       float64 // default 1
	RTSCTS         bool    // default false

	// tcp
	Host    string
	TCPPort int

	// bluetooth
	BDAddr            string
	Channel           string  // empty = SDP auto-detect
	Reconnect         bool    // default true
	ReconnectDelay    float64 // default 5
	ReconnectMaxDelay float64 // default 60

	// common
	OTABaudrate    int     // default 1200
	InitString     string  // raw, with \r \n escapes unresolved
	InitDelay      float64 // default 1.0
	SendKISSExit   bool    // default true
	HostExitString string
	ExitDelay      float64 // default 1.0

	AX25Version int // 20 or 22; default 22

	KISS kiss.Params // from [kiss.N]; nil fields = don't send
}

// Config is the parsed configuration.
type Config struct {
	Server Server
	AX25   AX25
	Ports  []Port
}

// knownServerKeys are the recognized keys in [server].
var knownServerKeys = []string{
	"listen_host", "listen_port", "callsign", "max_clients", "idle_timeout",
}

// knownAX25Keys are the recognized keys in [ax25].
var knownAX25Keys = []string{
	"max_window", "n2_retry", "t3_timeout",
}

// knownClientKeys are the recognized keys in [client.N].
var knownClientKeys = []string{
	"name", "type",
	"device", "serial_baudrate", "baudrate", "parity", "stopbits", "rtscts",
	"host", "port",
	"bdaddr", "channel", "reconnect", "reconnect_delay", "reconnect_max_delay",
	"ota_baudrate", "init_string", "init_delay", "send_kiss_exit",
	"host_exit_string", "exit_delay",
	"ax25_version",
}

// knownKISSKeys are the recognized keys in [kiss.N].
var knownKISSKeys = []string{
	"tx_delay", "persistence", "slot_time", "tx_tail", "full_duplex",
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	// dp[j] = distance from a[:i] to b[:j]
	dp := make([]int, lb+1)
	for j := range dp {
		dp[j] = j
	}
	for i := 1; i <= la; i++ {
		prev := dp[0]
		dp[0] = i
		for j := 1; j <= lb; j++ {
			tmp := dp[j]
			if a[i-1] == b[j-1] {
				dp[j] = prev
			} else {
				min3 := prev
				if dp[j-1]+1 < min3 {
					min3 = dp[j-1] + 1
				}
				if dp[j]+1 < min3 {
					min3 = dp[j] + 1
				}
				dp[j] = min3
			}
			prev = tmp
		}
	}
	return dp[lb]
}

// closestKey returns the closest known key within Levenshtein distance <= 2,
// or "" if none found.
func closestKey(key string, known []string) string {
	best := ""
	bestDist := 3 // only accept <= 2
	for _, k := range known {
		d := levenshtein(key, k)
		if d < bestDist {
			bestDist = d
			best = k
		}
	}
	return best
}

// warnUnknownKeys logs a warning for any key in a section that is not in the known list.
func warnUnknownKeys(section *ini.Section, known []string) {
	for _, key := range section.Keys() {
		name := key.Name()
		found := false
		for _, k := range known {
			if k == name {
				found = true
				break
			}
		}
		if !found {
			closest := closestKey(name, known)
			if closest != "" {
				log.Printf("warning: [%s] unknown key %q (did you mean %q?)", section.Name(), name, closest)
			} else {
				log.Printf("warning: [%s] unknown key %q", section.Name(), name)
			}
		}
	}
}

// getInt returns an int from a section key with a fallback default.
func getInt(s *ini.Section, key string, def int) int {
	if s.HasKey(key) {
		v, err := s.Key(key).Int()
		if err == nil {
			return v
		}
	}
	return def
}

// getFloat returns a float64 from a section key with a fallback default.
func getFloat(s *ini.Section, key string, def float64) float64 {
	if s.HasKey(key) {
		v, err := s.Key(key).Float64()
		if err == nil {
			return v
		}
	}
	return def
}

// getBool returns a bool from a section key with a fallback default.
func getBool(s *ini.Section, key string, def bool) bool {
	if s.HasKey(key) {
		v, err := s.Key(key).Bool()
		if err == nil {
			return v
		}
		// handle "true"/"false" already handled by ini, but also handle 0/1
		raw := s.Key(key).String()
		if raw == "0" {
			return false
		}
		if raw == "1" {
			return true
		}
	}
	return def
}

// getString returns the string value of a key, or the default if absent.
func getString(s *ini.Section, key string, def string) string {
	if s.HasKey(key) {
		return s.Key(key).String()
	}
	return def
}

// getIntPtr returns a *int from a section key, or nil if absent.
func getIntPtr(s *ini.Section, key string) *int {
	if s.HasKey(key) {
		v, err := s.Key(key).Int()
		if err == nil {
			return &v
		}
	}
	return nil
}

// Load reads and parses the INI file at path. If path is empty, returns
// defaults plus one serial port (client.0, /dev/ttyUSB0).
func Load(path string) (*Config, error) {
	// Configure ini to be lenient (allows inline comments, etc.)
	opts := ini.LoadOptions{
		IgnoreInlineComment:         true,
		UnescapeValueDoubleQuotes:   false,
		UnescapeValueCommentSymbols: false,
		InsensitiveKeys:             true,
	}

	var f *ini.File
	var err error

	if path == "" {
		// No config file: use defaults only.
		f = ini.Empty()
	} else {
		f, err = ini.LoadSources(opts, path)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
	}

	// --- Migrate bare [client] / [kiss] sections to numbered form ---
	if f.HasSection("client") {
		// Only migrate if there are no client.N sections
		hasNumbered := false
		for _, s := range f.Sections() {
			if len(s.Name()) > 7 && s.Name()[:7] == "client." {
				hasNumbered = true
				break
			}
		}
		if !hasNumbered {
			log.Printf("warning: [client] section is deprecated; rename to [client.0]")
			// Create client.0 and copy keys
			newSec, _ := f.NewSection("client.0")
			for _, k := range f.Section("client").Keys() {
				newSec.NewKey(k.Name(), k.Value())
			}
			f.DeleteSection("client")
		}
	}

	if f.HasSection("kiss") {
		// Only migrate if there are no kiss.N sections
		hasNumbered := false
		for _, s := range f.Sections() {
			if len(s.Name()) > 5 && s.Name()[:5] == "kiss." {
				hasNumbered = true
				break
			}
		}
		if !hasNumbered {
			log.Printf("warning: [kiss] section is deprecated; rename to [kiss.0]")
			newSec, _ := f.NewSection("kiss.0")
			for _, k := range f.Section("kiss").Keys() {
				newSec.NewKey(k.Name(), k.Value())
			}
			f.DeleteSection("kiss")
		}
	}

	// --- If no client.N sections, create a default client.0 ---
	hasClientSection := false
	for _, s := range f.Sections() {
		name := s.Name()
		if len(name) > 7 && name[:7] == "client." {
			hasClientSection = true
			break
		}
	}
	if !hasClientSection && path == "" {
		// CLI/default mode: add client.0 with serial defaults
		newSec, _ := f.NewSection("client.0")
		newSec.NewKey("type", "serial")
		newSec.NewKey("device", "/dev/ttyUSB0")
		newSec.NewKey("serial_baudrate", "9600")
		newSec.NewKey("ota_baudrate", "1200")
	}

	// --- Parse [server] ---
	serverSec := f.Section("server")
	warnUnknownKeys(serverSec, knownServerKeys)
	cfg := &Config{}
	cfg.Server = Server{
		ListenHost:  getString(serverSec, "listen_host", "127.0.0.1"),
		ListenPort:  getInt(serverSec, "listen_port", 8000),
		Callsign:    getString(serverSec, "callsign", "AGWPE"),
		MaxClients:  getInt(serverSec, "max_clients", 8),
		IdleTimeout: getInt(serverSec, "idle_timeout", 300),
	}

	// --- Parse [ax25] ---
	ax25Sec := f.Section("ax25")
	warnUnknownKeys(ax25Sec, knownAX25Keys)
	maxWindow := getInt(ax25Sec, "max_window", 3)
	if maxWindow < 1 {
		maxWindow = 1
	}
	if maxWindow > 7 {
		maxWindow = 7
	}
	cfg.AX25 = AX25{
		MaxWindow: maxWindow,
		N2Retry:   getInt(ax25Sec, "n2_retry", 10),
		T3Timeout: getInt(ax25Sec, "t3_timeout", 180),
	}

	// --- Collect client.N and kiss.N sections ---
	type portEntry struct {
		idx  int
		name string
	}
	var portEntries []portEntry
	kissMap := map[int]*ini.Section{}

	for _, s := range f.Sections() {
		name := s.Name()
		if len(name) > 7 && name[:7] == "client." {
			idxStr := name[7:]
			idx, err := strconv.Atoi(idxStr)
			if err == nil {
				portEntries = append(portEntries, portEntry{idx, name})
			}
		} else if len(name) > 5 && name[:5] == "kiss." {
			idxStr := name[5:]
			idx, err := strconv.Atoi(idxStr)
			if err == nil {
				kissMap[idx] = s
			}
		}
	}

	// Sort by index
	sort.Slice(portEntries, func(i, j int) bool {
		return portEntries[i].idx < portEntries[j].idx
	})

	// --- Validate contiguous numbering ---
	if len(portEntries) == 0 {
		return nil, fmt.Errorf("no [client.N] sections found; define at least [client.0]")
	}
	for i, pe := range portEntries {
		if pe.idx != i {
			actual := make([]int, len(portEntries))
			for j, e := range portEntries {
				actual[j] = e.idx
			}
			return nil, fmt.Errorf("port numbering must be contiguous starting from 0 (not contiguous): found %v", actual)
		}
	}

	// --- Parse each port ---
	cfg.Ports = make([]Port, len(portEntries))
	for i, pe := range portEntries {
		s := f.Section(pe.name)
		warnUnknownKeys(s, knownClientKeys)

		portType := getString(s, "type", "")
		if portType == "" {
			return nil, fmt.Errorf("[%s] missing required 'type' field", pe.name)
		}
		validTypes := map[string]bool{"serial": true, "tcp": true, "bluetooth": true}
		if !validTypes[portType] {
			return nil, fmt.Errorf("[%s] invalid type %q; must be one of: bluetooth, serial, tcp", pe.name, portType)
		}

		// Validate required fields per type
		switch portType {
		case "serial":
			if !s.HasKey("device") || s.Key("device").String() == "" {
				return nil, fmt.Errorf("[%s] type=serial is missing required field: device", pe.name)
			}
		case "tcp":
			missingFields := []string{}
			if !s.HasKey("host") || s.Key("host").String() == "" {
				missingFields = append(missingFields, "host")
			}
			if !s.HasKey("port") || s.Key("port").String() == "" {
				missingFields = append(missingFields, "port")
			}
			if len(missingFields) > 0 {
				return nil, fmt.Errorf("[%s] type=tcp is missing required field(s): %v", pe.name, missingFields)
			}
		case "bluetooth":
			if !s.HasKey("bdaddr") || s.Key("bdaddr").String() == "" {
				return nil, fmt.Errorf("[%s] type=bluetooth is missing required field: bdaddr", pe.name)
			}
		}

		// serial_baudrate with legacy fallback to "baudrate"
		serialBaudrate := 9600
		if s.HasKey("serial_baudrate") {
			if v, err := s.Key("serial_baudrate").Int(); err == nil {
				serialBaudrate = v
			}
		} else if s.HasKey("baudrate") {
			if v, err := s.Key("baudrate").Int(); err == nil {
				serialBaudrate = v
			}
		}

		ax25Version := 22
		if s.HasKey("ax25_version") {
			switch s.Key("ax25_version").String() {
			case "2.0":
				ax25Version = 20
			case "2.2":
				ax25Version = 22
			default:
				return nil, fmt.Errorf("[%s] invalid ax25_version %q; must be 2.0 or 2.2",
					pe.name, s.Key("ax25_version").String())
			}
		}

		port := Port{
			Name:              getString(s, "name", fmt.Sprintf("Port %d", i)),
			Type:              portType,
			Device:            getString(s, "device", ""),
			SerialBaudrate:    serialBaudrate,
			Parity:            getString(s, "parity", "N"),
			StopBits:          getFloat(s, "stopbits", 1),
			RTSCTS:            getBool(s, "rtscts", false),
			Host:              getString(s, "host", ""),
			TCPPort:           getInt(s, "port", 0),
			BDAddr:            getString(s, "bdaddr", ""),
			Channel:           getString(s, "channel", ""),
			Reconnect:         getBool(s, "reconnect", true),
			ReconnectDelay:    getFloat(s, "reconnect_delay", 5),
			ReconnectMaxDelay: getFloat(s, "reconnect_max_delay", 60),
			OTABaudrate:       getInt(s, "ota_baudrate", 1200),
			InitString:        getString(s, "init_string", ""),
			InitDelay:         getFloat(s, "init_delay", 1.0),
			SendKISSExit:      getBool(s, "send_kiss_exit", true),
			HostExitString:    getString(s, "host_exit_string", ""),
			ExitDelay:         getFloat(s, "exit_delay", 1.0),
			AX25Version:       ax25Version,
		}

		// Parse corresponding [kiss.N] section if present
		if ks, ok := kissMap[i]; ok {
			warnUnknownKeys(ks, knownKISSKeys)
			port.KISS = kiss.Params{
				TXDelay:     getIntPtr(ks, "tx_delay"),
				Persistence: getIntPtr(ks, "persistence"),
				SlotTime:    getIntPtr(ks, "slot_time"),
				TXTail:      getIntPtr(ks, "tx_tail"),
				FullDuplex:  getIntPtr(ks, "full_duplex"),
			}
		}

		cfg.Ports[i] = port
	}

	return cfg, nil
}

// Validate checks the config for logical errors and returns an error naming
// the section and key if any required fields are missing or invalid.
// Load already performs all validation; this method is provided for
// post-load re-validation after programmatic modification.
func (c *Config) Validate() error {
	validTypes := map[string]bool{"serial": true, "tcp": true, "bluetooth": true}
	for i, p := range c.Ports {
		secName := fmt.Sprintf("client.%d", i)
		if p.Type == "" {
			return fmt.Errorf("[%s] missing required 'type' field", secName)
		}
		if !validTypes[p.Type] {
			return fmt.Errorf("[%s] invalid type %q; must be one of: bluetooth, serial, tcp", secName, p.Type)
		}
		switch p.Type {
		case "serial":
			if p.Device == "" {
				return fmt.Errorf("[%s] type=serial is missing required field: device", secName)
			}
		case "tcp":
			if p.Host == "" {
				return fmt.Errorf("[%s] type=tcp is missing required field: host", secName)
			}
			if p.TCPPort == 0 {
				return fmt.Errorf("[%s] type=tcp is missing required field: port", secName)
			}
		case "bluetooth":
			if p.BDAddr == "" {
				return fmt.Errorf("[%s] type=bluetooth is missing required field: bdaddr", secName)
			}
		}
	}
	return nil
}
