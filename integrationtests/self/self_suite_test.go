package self_test

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	mrand "math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/internal/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const alpn = "quic-go integration tests"

const (
	dataLen     = 500 * 1024       // 500 KB
	dataLenLong = 50 * 1024 * 1024 // 50 MB
)

var (
	// PRData contains dataLen bytes of pseudo-random data.
	PRData = GeneratePRData(dataLen)
	// PRDataLong contains dataLenLong bytes of pseudo-random data.
	PRDataLong = GeneratePRData(dataLenLong)
)

// See https://en.wikipedia.org/wiki/Lehmer_random_number_generator
func GeneratePRData(l int) []byte {
	res := make([]byte, l)
	seed := uint64(1)
	for i := 0; i < l; i++ {
		seed = seed * 48271 % 2147483647
		res[i] = byte(seed)
	}
	return res
}

const logBufSize = 100 * 1 << 20 // initial size of the log buffer: 100 MB

type syncedBuffer struct {
	mutex sync.Mutex

	*bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mutex.Lock()
	n, err := b.Buffer.Write(p)
	b.mutex.Unlock()
	return n, err
}

func (b *syncedBuffer) Bytes() []byte {
	b.mutex.Lock()
	p := b.Buffer.Bytes()
	b.mutex.Unlock()
	return p
}

func (b *syncedBuffer) Reset() {
	b.mutex.Lock()
	b.Buffer.Reset()
	b.mutex.Unlock()
}

var (
	logFileName string // the log file set in the ginkgo flags
	logBufOnce  sync.Once
	logBuf      *syncedBuffer
	enableQlog  bool

	tlsConfig          *tls.Config
	tlsConfigLongChain *tls.Config
	tlsClientConfig    *tls.Config
)

// read the logfile command line flag
// to set call ginkgo -- -logfile=log.txt
func init() {
	flag.StringVar(&logFileName, "logfile", "", "log file")
	flag.BoolVar(&enableQlog, "qlog", false, "enable qlog")

	ca, caPrivateKey, err := generateCA()
	if err != nil {
		panic(err)
	}
	leafCert, leafPrivateKey, err := generateLeafCert(ca, caPrivateKey)
	if err != nil {
		panic(err)
	}
	tlsConfig = &tls.Config{
		Certificates: []tls.Certificate{tls.Certificate{
			Certificate: [][]byte{leafCert.Raw},
			PrivateKey:  leafPrivateKey,
		}},
		NextProtos: []string{alpn},
	}
	tlsConfLongChain, err := generateTLSConfigWithLongCertChain(ca, caPrivateKey)
	if err != nil {
		panic(err)
	}
	tlsConfigLongChain = tlsConfLongChain

	root := x509.NewCertPool()
	root.AddCert(ca)
	tlsClientConfig = &tls.Config{
		RootCAs:    root,
		NextProtos: []string{alpn},
	}
}

func generateCA() (*x509.Certificate, *rsa.PrivateKey, error) {
	certTempl := &x509.Certificate{
		SerialNumber:          big.NewInt(2019),
		Subject:               pkix.Name{},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, certTempl, certTempl, &caPrivateKey.PublicKey, caPrivateKey)
	if err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(caBytes)
	if err != nil {
		return nil, nil, err
	}
	return ca, caPrivateKey, nil
}

func generateLeafCert(ca *x509.Certificate, caPrivateKey *rsa.PrivateKey) (*x509.Certificate, *rsa.PrivateKey, error) {
	certTempl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, certTempl, ca, &privKey.PublicKey, caPrivateKey)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, privKey, nil
}

// getTLSConfigWithLongCertChain generates a tls.Config that uses a long certificate chain.
// The Root CA used is the same as for the config returned from getTLSConfig().
func generateTLSConfigWithLongCertChain(ca *x509.Certificate, caPrivateKey *rsa.PrivateKey) (*tls.Config, error) {
	const chainLen = 7
	certTempl := &x509.Certificate{
		SerialNumber:          big.NewInt(2019),
		Subject:               pkix.Name{},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}

	lastCA := ca
	lastCAPrivKey := caPrivateKey
	certs := make([]*x509.Certificate, chainLen)
	for i := 0; i < chainLen; i++ {
		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, err
		}
		caBytes, err := x509.CreateCertificate(rand.Reader, certTempl, lastCA, &privKey.PublicKey, lastCAPrivKey)
		if err != nil {
			return nil, err
		}
		ca, err := x509.ParseCertificate(caBytes)
		if err != nil {
			return nil, err
		}
		certs[i] = ca
		lastCA = ca
		lastCAPrivKey = privKey
	}
	leafCert, leafPrivateKey, err := generateLeafCert(lastCA, lastCAPrivKey)
	if err != nil {
		return nil, err
	}

	rawCerts := make([][]byte, chainLen+1)
	for i, cert := range certs {
		rawCerts[chainLen-i] = cert.Raw
	}
	rawCerts[0] = leafCert.Raw

	return &tls.Config{
		Certificates: []tls.Certificate{tls.Certificate{
			Certificate: rawCerts,
			PrivateKey:  leafPrivateKey,
		}},
		NextProtos: []string{alpn},
	}, nil
}

func getTLSConfig() *tls.Config {
	return tlsConfig.Clone()
}

func getTLSConfigWithLongCertChain() *tls.Config {
	return tlsConfigLongChain.Clone()
}

func getTLSClientConfig() *tls.Config {
	return tlsClientConfig.Clone()
}

func getQuicConfigForClient(conf *quic.Config) *quic.Config {
	return getQuicConfigForRole("client", conf)
}

func getQuicConfigForServer(conf *quic.Config) *quic.Config {
	return getQuicConfigForRole("server", conf)
}

func getQuicConfigForRole(role string, conf *quic.Config) *quic.Config {
	if conf == nil {
		conf = &quic.Config{}
	} else {
		conf = conf.Clone()
	}
	if !enableQlog {
		return conf
	}
	conf.GetLogWriter = func(connectionID []byte) io.WriteCloser {
		filename := fmt.Sprintf("log_%x_%s.qlog", connectionID, role)
		fmt.Fprintf(GinkgoWriter, "Creating %s.\n", filename)
		f, err := os.Create(filename)
		Expect(err).ToNot(HaveOccurred())
		bw := bufio.NewWriter(f)
		return utils.NewBufferedWriteCloser(bw, f)
	}
	return conf
}

var _ = BeforeEach(func() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if debugLog() {
		logBufOnce.Do(func() {
			logBuf = &syncedBuffer{Buffer: bytes.NewBuffer(make([]byte, 0, logBufSize))}
		})
		utils.DefaultLogger.SetLogLevel(utils.LogLevelDebug)
		log.SetOutput(logBuf)
	}
})

var _ = AfterEach(func() {
	if debugLog() {
		logFile, err := os.Create(logFileName)
		Expect(err).ToNot(HaveOccurred())
		logFile.Write(logBuf.Bytes())
		logFile.Close()
		logBuf.Reset()
	}
})

// Debug says if this test is being logged
func debugLog() bool {
	return len(logFileName) > 0
}

func TestSelf(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Self integration tests")
}

var _ = BeforeSuite(func() {
	mrand.Seed(GinkgoRandomSeed())
})
