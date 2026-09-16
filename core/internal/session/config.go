package session

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"time"
	b "veil.local/core/internal/behavior"
)

const maxObject = 256 << 10

var objectETag = regexp.MustCompile(`^"[a-fA-F0-9]{32}"$`)

type Bundle struct {
	Version       int
	SeedHex       string
	Model         b.BatchModel
	InnerProtocol string `json:",omitempty"`
}

func GenerateBundle() (Bundle, error) {
	var seed [32]byte
	if _, e := rand.Read(seed[:]); e != nil {
		return Bundle{}, e
	}
	return Bundle{Version: 1, SeedHex: hex.EncodeToString(seed[:]), Model: adaptiveModel()}, nil
}
func GenerateMuxBundle() (Bundle, error) {
	return GenerateMuxBundleProfile(ProfileAdaptive64)
}
func GenerateMuxBundleProfile(profile string) (Bundle, error) {
	if profile != ProfileAdaptive64 && profile != ProfileBulk192 {
		return Bundle{}, errors.New("unknown model profile")
	}
	b, e := GenerateBundle()
	if e != nil {
		return b, e
	}
	b.Version = 2
	b.InnerProtocol = "streammux-v1"
	if profile == ProfileBulk192 {
		b.Model = bulk192Model()
	}
	return b, nil
}
func LoadBundle(path string) (Bundle, [32]byte, *b.BatchProgram, error) {
	var bundle Bundle
	var seed [32]byte
	if e := ReadPrivateJSON(path, &bundle, 256<<10); e != nil {
		return bundle, seed, nil, e
	}
	raw, e := hex.DecodeString(bundle.SeedHex)
	if e != nil || len(raw) != 32 || !(bundle.Version == 1 && bundle.InnerProtocol == "" || bundle.Version == 2 && bundle.InnerProtocol == "streammux-v1") {
		return bundle, seed, nil, errors.New("model bundle version or seed")
	}
	copy(seed[:], raw)
	program, e := b.CompileBatch(bundle.Model)
	if e != nil {
		return bundle, seed, nil, e
	}
	supported, e := b.CompileBatch(adaptiveModel())
	if e != nil {
		return bundle, seed, nil, e
	}
	accepted := program.ID() == supported.ID()
	if !accepted && bundle.Version == 2 {
		bulk, compileErr := b.CompileBatch(bulk192Model())
		if compileErr != nil {
			return bundle, seed, nil, compileErr
		}
		accepted = program.ID() == bulk.ID()
	}
	if !accepted {
		return bundle, seed, nil, errors.New("model is not supported by this object adapter")
	}
	return bundle, seed, program, nil
}
func WriteBundle(path string, bundle Bundle) error { return writePrivateBundle(path, bundle) }
func writePrivateBundle(path string, bundle any) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	e = enc.Encode(bundle)
	closeErr := f.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func privateFile(path string, limit int64) error {
	st, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > limit {
		return errors.New("private file permission, type or size")
	}
	return nil
}
func ReadPrivateJSON(path string, value any, limit int64) error {
	if e := privateFile(path, limit); e != nil {
		return e
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, limit+1))
	d.DisallowUnknownFields()
	if e = d.Decode(value); e != nil {
		return e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func boundedFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("file type or size")
	}
	p, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(p)) > limit {
		return nil, errors.New("file size")
	}
	return p, e
}
func roots(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	p, e := boundedFile(path, 1<<20)
	if e != nil {
		return nil, e
	}
	if len(p) > 1<<20 {
		return nil, errors.New("CA file bound")
	}
	r := x509.NewCertPool()
	if !r.AppendCertsFromPEM(p) {
		return nil, errors.New("invalid CA")
	}
	return r, nil
}
func certificate(cert, key string) (tls.Certificate, error) {
	if e := privateFile(key, 64<<10); e != nil {
		return tls.Certificate{}, e
	}
	certPEM, e := boundedFile(cert, 1<<20)
	if e != nil {
		return tls.Certificate{}, e
	}
	keyPEM, e := boundedFile(key, 64<<10)
	if e != nil {
		return tls.Certificate{}, e
	}
	c, e := tls.X509KeyPair(certPEM, keyPEM)
	if e != nil {
		return c, e
	}
	c.Leaf, e = x509.ParseCertificate(c.Certificate[0])
	if e != nil {
		return c, e
	}
	now := time.Now()
	if now.Before(c.Leaf.NotBefore) || !now.Before(c.Leaf.NotAfter) {
		return c, errors.New("certificate outside validity")
	}
	return c, nil
}
func endpoint(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("HTTPS endpoint must have only scheme and host")
	}
	u.Path = ""
	if u.Hostname() == "" {
		return nil, errors.New("endpoint hostname")
	}
	if u.Port() != "" {
		n, e := strconv.Atoi(u.Port())
		if e != nil || n < 1 || n > 65535 {
			return nil, errors.New("endpoint port")
		}
	}
	return u, nil
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func validateListen(address string, loopback bool) error {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return e
	}
	ip, e := netip.ParseAddr(host)
	if e != nil || ip.Zone() != "" || loopback && !ip.Unmap().IsLoopback() {
		return errors.New("listener must use a numeric permitted address")
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 65535 {
		return errors.New("listener port")
	}
	return nil
}

type ServerConfig struct {
	BackendMode                                                 string
	Mux                                                         MuxConfig
	MaxActiveStreams, MaxActiveStreamsPerIdentity               int
	UDPMaxTargets, UDPIdleMS                                    int
	DNSAddress                                                  string
	AllowCIDRs, DenyCIDRs                                       []string
	Version                                                     int
	Listen, ModelFile, Certificate, PrivateKey, ClientCA        string
	ClientFingerprints                                          []string
	BackendURL, BackendCA, BackendAccess, BackendSecret, Bucket string
	MaxConnections, MaxSessions, MaxSessionsPerIdentity, Window int
	MaxBytes                                                    uint64
	ConnectTimeoutMS, IdleTimeoutMS                             int
}

func (c *ServerConfig) defaults() error {
	if c.Version < 1 || c.Version > 5 {
		return errors.New("server configuration version")
	}
	if c.UDPMaxTargets == 0 {
		c.UDPMaxTargets = 8
	}
	if c.UDPIdleMS == 0 {
		c.UDPIdleMS = 30000
	}
	if c.UDPMaxTargets < 1 || c.UDPMaxTargets > 16 || c.UDPIdleMS < 100 || c.UDPIdleMS > 600000 {
		return errors.New("UDP resource limits")
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 32
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = min(8, c.MaxConnections)
	}
	if c.MaxSessionsPerIdentity == 0 {
		c.MaxSessionsPerIdentity = min(2, c.MaxSessions)
	}
	if c.Window == 0 {
		c.Window = 1 << 20
		if c.Version >= 2 {
			c.Window = 64 << 10
		}
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 8 << 20
	}
	if c.ConnectTimeoutMS == 0 {
		c.ConnectTimeoutMS = 3000
	}
	if c.IdleTimeoutMS == 0 {
		c.IdleTimeoutMS = 120000
	}
	if c.MaxConnections < 1 || c.MaxConnections > 256 || c.MaxSessions < 1 || c.MaxSessions > 32 || c.MaxSessions > c.MaxConnections || c.MaxSessionsPerIdentity < 1 || c.MaxSessionsPerIdentity > c.MaxSessions || c.Window < 16384 || c.Window > 1<<20 || c.MaxBytes < 1 || c.MaxBytes > 1<<40 || c.ConnectTimeoutMS < 1 || c.ConnectTimeoutMS > 30000 || c.IdleTimeoutMS < 100 || c.IdleTimeoutMS > 600000 || len(c.ClientFingerprints) < 1 || len(c.ClientFingerprints) > 128 {
		return errors.New("server resource limits")
	}
	if !bucketName.MatchString(c.Bucket) || c.ClientCA == "" {
		return errors.New("backend or authentication configuration")
	}
	if c.BackendMode == "" {
		c.BackendMode = "remote-s3"
	}
	switch c.BackendMode {
	case "remote-s3":
		if c.Version >= 3 {
			return errors.New("flight requires local-object-v1")
		}
		if c.BackendAccess == "" || c.BackendSecret == "" || len(c.BackendAccess) > 128 || len(c.BackendSecret) > 256 {
			return errors.New("remote backend credentials")
		}
	case "local-object-v1":
		if c.Version < 2 || c.BackendURL != "" || c.BackendCA != "" || c.BackendAccess != "" || c.BackendSecret != "" {
			return errors.New("local object mode requires configuration version2 through version5 and no remote backend configuration")
		}
	default:
		return errors.New("unsupported backend mode")
	}
	if c.Version >= 2 {
		if e := c.Mux.defaults(c.Window, c.MaxBytes); e != nil {
			return e
		}
		if c.MaxActiveStreams == 0 {
			c.MaxActiveStreams = min(256, c.MaxSessions*c.Mux.Streams)
		}
		if c.MaxActiveStreamsPerIdentity == 0 {
			c.MaxActiveStreamsPerIdentity = min(c.MaxActiveStreams, c.MaxSessionsPerIdentity*c.Mux.Streams)
		}
		if c.MaxActiveStreams < 1 || c.MaxActiveStreams > 256 || c.MaxActiveStreamsPerIdentity < 1 || c.MaxActiveStreamsPerIdentity > c.MaxActiveStreams {
			return errors.New("global stream admission limits")
		}
	} else if c.Mux != (MuxConfig{}) || c.MaxActiveStreams != 0 || c.MaxActiveStreamsPerIdentity != 0 {
		return errors.New("mux limits require configuration version2")
	}
	return validateListen(c.Listen, false)
}

type ClientConfig struct {
	Mux                                                                            MuxConfig
	MaxCarriers                                                                    int
	Version                                                                        int
	Listen, ServerURL, DialAddress, CA, Certificate, PrivateKey, ModelFile, Bucket string
	MaxConnections, Window                                                         int
	MaxBytes                                                                       uint64
	IdleTimeoutMS                                                                  int
}

func (c *ClientConfig) defaults() error {
	if c.Version < 1 || c.Version > 5 {
		return errors.New("client configuration version")
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 8
	}
	if c.Window == 0 {
		c.Window = 1 << 20
		if c.Version >= 2 {
			c.Window = 64 << 10
		}
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 8 << 20
	}
	if c.IdleTimeoutMS == 0 {
		c.IdleTimeoutMS = 120000
	}
	if c.MaxConnections < 1 || c.MaxConnections > 32 || c.Window < 16384 || c.Window > 1<<20 || c.MaxBytes < 1 || c.MaxBytes > 1<<40 || c.IdleTimeoutMS < 100 || c.IdleTimeoutMS > 600000 || !bucketName.MatchString(c.Bucket) {
		return errors.New("client resource limits")
	}
	if _, e := endpoint(c.ServerURL); e != nil {
		return e
	}
	if c.DialAddress != "" {
		if a, e := netip.ParseAddrPort(c.DialAddress); e != nil || a.Port() == 0 || a.Addr().Zone() != "" {
			return errors.New("dial_address must be numeric with a port")
		}
	}
	if c.Version >= 2 {
		if e := c.Mux.defaults(c.Window, c.MaxBytes); e != nil {
			return e
		}
		if c.MaxCarriers == 0 {
			c.MaxCarriers = 2
		}
		if c.MaxCarriers < 1 || c.MaxCarriers > 8 || c.MaxCarriers > c.MaxConnections {
			return errors.New("carrier pool limit")
		}
	} else if c.Mux != (MuxConfig{}) || c.MaxCarriers != 0 {
		return errors.New("mux limits require configuration version2")
	}
	return validateListen(c.Listen, true)
}
