package client

import (
	cryptoRand "crypto/rand"
	"encoding/binary"
	utls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"math/big"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/cbeuw/Cloak/internal/common"
)

const appDataMaxLength = 16401

type clientHelloFields struct {
	random         []byte
	sessionId      []byte
	x25519KeyShare []byte
	serverName     string
}

type browser int

const (
	chrome = iota
	firefox
	safari
)

func generateSNI(serverName string) []byte {
	serverNameListLength := make([]byte, 2)
	binary.BigEndian.PutUint16(serverNameListLength, uint16(len(serverName)+3))
	serverNameType := []byte{0x00} // host_name
	serverNameLength := make([]byte, 2)
	binary.BigEndian.PutUint16(serverNameLength, uint16(len(serverName)))
	ret := make([]byte, 2+1+2+len(serverName))
	copy(ret[0:2], serverNameListLength)
	copy(ret[2:3], serverNameType)
	copy(ret[3:5], serverNameLength)
	copy(ret[5:], serverName)
	return ret
}

type DirectTLS struct {
	*common.TLSConn
	browser browser
}

var topLevelDomains = []string{"com", "net", "org", "it", "fr", "me", "ru", "cn", "es", "tr", "top", "xyz", "info"}

// https://github.com/ProtonVPN/wireguard-go/commit/bcf344b39b213c1f32147851af0d2a8da9266883
func randomServerName() string {
	charNum := int('z') - int('a') + 1
	size := 3 + randInt(10)
	name := make([]byte, size)
	for i := range name {
		name[i] = byte(int('a') + randInt(charNum))
	}
	return string(name) + "." + randItem(topLevelDomains)
}

func randItem(list []string) string {
	return list[randInt(len(list))]
}

func randInt(n int) int {
	size, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(int64(n)))
	if err == nil {
		return int(size.Int64())
	}
	//goland:noinspection GoDeprecation
	rand.Seed(time.Now().UnixNano())
	return rand.Intn(n)
}

func buildClientHello(browser browser, fields clientHelloFields) ([]byte, error) {
	// We don't use utls to handle connections (as it'll attempt a real TLS negotiation)
	// We only want it to build the ClientHello locally
	fakeConn := net.TCPConn{}
	var helloID utls.ClientHelloID
	switch browser {
	case chrome:
		helloID = utls.HelloChrome_Auto
	case firefox:
		helloID = utls.HelloFirefox_Auto
	case safari:
		helloID = utls.HelloSafari_Auto
	}

	uclient := utls.UClient(&fakeConn, &utls.Config{ServerName: fields.serverName}, helloID)
	if err := uclient.BuildHandshakeState(); err != nil {
		return []byte{}, err
	}
	if err := uclient.SetClientRandom(fields.random); err != nil {
		return []byte{}, err
	}

	uclient.HandshakeState.Hello.SessionId = make([]byte, 32)
	copy(uclient.HandshakeState.Hello.SessionId, fields.sessionId)

	// Find the X25519 key share and overwrite it
	var extIndex int
	var keyShareIndex int
	for i, ext := range uclient.Extensions {
		ext, ok := ext.(*utls.KeyShareExtension)
		if ok {
			extIndex = i
			for j, keyShare := range ext.KeyShares {
				if keyShare.Group == utls.X25519 {
					keyShareIndex = j
				}
			}
		}
	}
	copy(uclient.Extensions[extIndex].(*utls.KeyShareExtension).KeyShares[keyShareIndex].Data, fields.x25519KeyShare)

	if err := uclient.BuildHandshakeState(); err != nil {
		return []byte{}, err
	}
	return uclient.HandshakeState.Hello.Raw, nil
}

// Handshake handles the TLS handshake for a given conn and returns the sessionKey
// if the server proceed with Cloak authentication
func (tls *DirectTLS) Handshake(rawConn net.Conn, authInfo AuthInfo) (sessionKey [32]byte, err error) {
	payload, sharedSecret := makeAuthenticationPayload(authInfo)

	// random is marshalled ephemeral pub key 32 bytes
	// The authentication ciphertext and its tag are then distributed among SessionId and X25519KeyShare
	fields := clientHelloFields{
		random:         payload.randPubKey[:],
		sessionId:      payload.ciphertextWithTag[0:32],
		x25519KeyShare: payload.ciphertextWithTag[32:64],
		serverName:     authInfo.MockDomain,
	}

	if strings.EqualFold(fields.serverName, "random") {
		fields.serverName = randomServerName()
	}

	var ch []byte
	ch, err = buildClientHello(tls.browser, fields)
	if err != nil {
		return
	}
	chWithRecordLayer := common.AddRecordLayer(ch, common.Handshake, common.VersionTLS11)
	_, err = rawConn.Write(chWithRecordLayer)
	if err != nil {
		return
	}
	log.Trace("client hello sent successfully")
	tls.TLSConn = common.NewTLSConn(rawConn)

	buf := make([]byte, 1024)
	log.Trace("waiting for ServerHello")
	_, err = tls.Read(buf)
	if err != nil {
		return
	}

	encrypted := append(buf[6:38], buf[84:116]...)
	nonce := encrypted[0:12]
	ciphertextWithTag := encrypted[12:60]
	sessionKeySlice, err := common.AESGCMDecrypt(nonce, sharedSecret[:], ciphertextWithTag)
	if err != nil {
		return
	}
	copy(sessionKey[:], sessionKeySlice)

	for i := 0; i < 2; i++ {
		// ChangeCipherSpec and EncryptedCert (in the format of application data)
		_, err = tls.Read(buf)
		if err != nil {
			return
		}
	}
	return sessionKey, nil

}
