package pss

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/swarm/network"
	"github.com/ethereum/go-ethereum/swarm/storage"
	whisper "github.com/ethereum/go-ethereum/whisper/whisperv5"
)

const (
	pssServiceName = "pss"
	bzzServiceName = "bzz"
)

var (
	initOnce         = sync.Once{}
	snapshotfile     string
	debugdebugflag   = flag.Bool("vv", false, "veryverbose")
	debugflag        = flag.Bool("v", false, "verbose")
	snapshotflag     = flag.String("s", "", "snapshot filename")
	messagesflag     = flag.Int("m", 0, "number of messages to generate (default = number of nodes). Ignored if -s is not set")
	addresssizeflag  = flag.Int("b", 32, "number of bytes to use for address. Ignored if -s is not set")
	adaptertypeflag  = flag.String("a", "sim", "Adapter type to use. Ignored if -s is not set")
	messagedelayflag = flag.Int("d", 1000, "Message max delay period, in ms")
	w                *whisper.Whisper
	wapi             *whisper.PublicWhisperAPI
	psslogmain       log.Logger
	pssprotocols     map[string]*protoCtrl
	useHandshake     bool
)

var services = newServices()

func init() {

	flag.Parse()
	rand.Seed(time.Now().Unix())

	adapters.RegisterServices(services)

	initOnce.Do(
		func() {
			loglevel := log.LvlInfo
			if *debugflag {
				loglevel = log.LvlDebug
			} else if *debugdebugflag {
				loglevel = log.LvlTrace
			}

			psslogmain = log.New("psslog", "*")
			hs := log.StreamHandler(os.Stderr, log.TerminalFormat(true))
			hf := log.LvlFilterHandler(loglevel, hs)
			h := log.CallerFileHandler(hf)
			log.Root().SetHandler(h)

			w = whisper.New(&whisper.DefaultConfig)
			wapi = whisper.NewPublicWhisperAPI(w)

			pssprotocols = make(map[string]*protoCtrl)
		},
	)
}

// test if we can insert into cache, match items with cache and cache expiry
func TestCache(t *testing.T) {
	var err error
	to, _ := hex.DecodeString("08090a0b0c0d0e0f1011121314150001020304050607161718191a1b1c1d1e1f")
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	privkey, err := w.GetPrivateKey(keys)
	if err != nil {
		t.Fatal(err)
	}
	ps := newTestPss(privkey, nil)
	pp := NewPssParams(privkey)
	data := []byte("foo")
	datatwo := []byte("bar")
	wparams := &whisper.MessageParams{
		TTL:      defaultWhisperTTL,
		Src:      privkey,
		Dst:      &privkey.PublicKey,
		Topic:    whisper.TopicType(PingTopic),
		WorkTime: defaultWhisperWorkTime,
		PoW:      defaultWhisperPoW,
		Payload:  data,
	}
	woutmsg, err := whisper.NewSentMessage(wparams)
	env, err := woutmsg.Wrap(wparams)
	msg := &PssMsg{
		Payload: env,
		To:      to,
	}
	wparams.Payload = datatwo
	woutmsg, err = whisper.NewSentMessage(wparams)
	envtwo, err := woutmsg.Wrap(wparams)
	msgtwo := &PssMsg{
		Payload: envtwo,
		To:      to,
	}

	digest, err := ps.storeMsg(msg)
	if err != nil {
		t.Fatalf("could not store cache msgone: %v", err)
	}
	digesttwo, err := ps.storeMsg(msgtwo)
	if err != nil {
		t.Fatalf("could not store cache msgtwo: %v", err)
	}

	if digest == digesttwo {
		t.Fatalf("different msgs return same hash: %d", digesttwo)
	}

	// check the cache
	err = ps.addFwdCache(digest)
	if err != nil {
		t.Fatalf("write to pss expire cache failed: %v", err)
	}

	if !ps.checkFwdCache(nil, digest) {
		t.Fatalf("message %v should have EXPIRE record in cache but checkCache returned false", msg)
	}

	if ps.checkFwdCache(nil, digesttwo) {
		t.Fatalf("message %v should NOT have EXPIRE record in cache but checkCache returned true", msgtwo)
	}

	time.Sleep(pp.CacheTTL)
	if ps.checkFwdCache(nil, digest) {
		t.Fatalf("message %v should have expired from cache but checkCache returned true", msg)
	}
}

// matching of address hints; whether a message could be or is for the node
func TestAddressMatch(t *testing.T) {

	localaddr := network.RandomAddr().Over()
	copy(localaddr[:8], []byte("deadbeef"))
	remoteaddr := []byte("feedbeef")
	kadparams := network.NewKadParams()
	kad := network.NewKademlia(localaddr, kadparams)
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	if err != nil {
		t.Fatalf("Could not generate private key: %v", err)
	}
	privkey, err := w.GetPrivateKey(keys)
	pssp := NewPssParams(privkey)
	ps := NewPss(kad, nil, pssp)

	pssmsg := &PssMsg{
		To:      remoteaddr,
		Payload: &whisper.Envelope{},
	}

	// differ from first byte
	if ps.isSelfRecipient(pssmsg) {
		t.Fatalf("isSelfRecipient true but %x != %x", remoteaddr, localaddr)
	}
	if ps.isSelfPossibleRecipient(pssmsg) {
		t.Fatalf("isSelfPossibleRecipient true but %x != %x", remoteaddr[:8], localaddr[:8])
	}

	// 8 first bytes same
	copy(remoteaddr[:4], localaddr[:4])
	if ps.isSelfRecipient(pssmsg) {
		t.Fatalf("isSelfRecipient true but %x != %x", remoteaddr, localaddr)
	}
	if !ps.isSelfPossibleRecipient(pssmsg) {
		t.Fatalf("isSelfPossibleRecipient false but %x == %x", remoteaddr[:8], localaddr[:8])
	}

	// all bytes same
	pssmsg.To = localaddr
	if !ps.isSelfRecipient(pssmsg) {
		t.Fatalf("isSelfRecipient false but %x == %x", remoteaddr, localaddr)
	}
	if !ps.isSelfPossibleRecipient(pssmsg) {
		t.Fatalf("isSelfPossibleRecipient false but %x == %x", remoteaddr[:8], localaddr[:8])
	}
}

// set and generate pubkeys and symkeys
func TestKeys(t *testing.T) {
	// make our key and init pss with it
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	ourkeys, err := wapi.NewKeyPair(ctx)
	if err != nil {
		t.Fatalf("create 'our' key fail")
	}
	ctx, _ = context.WithTimeout(context.Background(), time.Second)
	theirkeys, err := wapi.NewKeyPair(ctx)
	if err != nil {
		t.Fatalf("create 'their' key fail")
	}
	ourprivkey, err := w.GetPrivateKey(ourkeys)
	if err != nil {
		t.Fatalf("failed to retrieve 'our' private key")
	}
	theirprivkey, err := w.GetPrivateKey(theirkeys)
	if err != nil {
		t.Fatalf("failed to retrieve 'their' private key")
	}
	ps := newTestPss(ourprivkey, nil)

	// set up peer with mock address, mapped to mocked publicaddress and with mocked symkey
	addr := make(PssAddress, 32)
	copy(addr, network.RandomAddr().Over())
	outkey := network.RandomAddr().Over()
	topic := BytesToTopic([]byte("foo:42"))
	ps.SetPeerPublicKey(&theirprivkey.PublicKey, topic, &addr)
	outkeyid, err := ps.SetSymmetricKey(outkey, topic, &addr, false)
	if err != nil {
		t.Fatalf("failed to set 'our' outgoing symmetric key")
	}

	// make a symmetric key that we will send to peer for encrypting messages to us
	inkeyid, err := ps.generateSymmetricKey(topic, &addr, true)
	if err != nil {
		t.Fatalf("failed to set 'our' incoming symmetric key")
	}

	// get the key back from whisper, check that it's still the same
	outkeyback, err := ps.w.GetSymKey(outkeyid)
	if err != nil {
		t.Fatalf(err.Error())
	}
	inkey, err := ps.w.GetSymKey(inkeyid)
	if err != nil {
		t.Fatalf(err.Error())
	}
	if !bytes.Equal(outkeyback, outkey) {
		t.Fatalf("passed outgoing symkey doesnt equal stored: %x / %x", outkey, outkeyback)
	}

	t.Logf("symout: %v", outkeyback)
	t.Logf("symin: %v", inkey)

	// check that the key is stored in the peerpool
	//psp := ps.symKeyPool[inkeyid][topic]
}

// send symmetrically encrypted message between two directly connected peers
func TestSymSend(t *testing.T) {
	t.Run("32", testSymSend)
	t.Run("8", testSymSend)
	t.Run("0", testSymSend)
}

func testSymSend(t *testing.T) {

	// address hint size
	var addrsize int64
	var err error
	paramstring := strings.Split(t.Name(), "/")
	addrsize, _ = strconv.ParseInt(paramstring[1], 10, 0)
	log.Info("sym send test", "addrsize", addrsize)

	topic := BytesToTopic([]byte("foo:42"))
	topichex := hexutil.Encode(topic[:])

	clients, err := setupNetwork(2)
	if err != nil {
		t.Fatal(err)
	}

	//loaddr := make([]byte, addrsize)
	var loaddrhex string
	err = clients[0].Call(&loaddrhex, "pss_baseAddr")
	if err != nil {
		t.Fatalf("rpc get node 1 baseaddr fail: %v", err)
	}
	var roaddrhex string
	err = clients[1].Call(&roaddrhex, "pss_baseAddr")
	if err != nil {
		t.Fatalf("rpc get node 2 baseaddr fail: %v", err)
	}

	// retrieve public key from pss instance
	// set this public key reciprocally
	var lpubkeyhex string
	err = clients[0].Call(&lpubkeyhex, "pss_getPublicKey")
	if err != nil {
		t.Fatalf("rpc get node 1 pubkey fail: %v", err)
	}
	var rpubkeyhex string
	err = clients[1].Call(&rpubkeyhex, "pss_getPublicKey")
	if err != nil {
		t.Fatalf("rpc get node 2 pubkey fail: %v", err)
	}

	time.Sleep(time.Millisecond * 500)

	// at this point we've verified that symkeys are saved and match on each peer
	// now try sending symmetrically encrypted message, both directions
	lmsgC := make(chan APIMsg)
	lctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	lsub, err := clients[0].Subscribe(lctx, "pss", lmsgC, "receive", topichex)
	log.Trace("lsub", "id", lsub)
	defer lsub.Unsubscribe()
	rmsgC := make(chan APIMsg)
	rctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	rsub, err := clients[1].Subscribe(rctx, "pss", rmsgC, "receive", topichex)
	log.Trace("rsub", "id", rsub)
	defer rsub.Unsubscribe()

	lrecvkey := network.RandomAddr().Over()
	rrecvkey := network.RandomAddr().Over()

	var lkeyids [2]string
	var rkeyids [2]string

	// manually set reciprocal symkeys
	err = clients[0].Call(&lkeyids, "psstest_setSymKeys", rpubkeyhex, lrecvkey, rrecvkey, defaultSymKeySendLimit, topichex, roaddrhex)
	if err != nil {
		t.Fatal(err)
	}
	err = clients[1].Call(&rkeyids, "psstest_setSymKeys", rpubkeyhex, rrecvkey, lrecvkey, defaultSymKeySendLimit, topichex, loaddrhex)
	if err != nil {
		t.Fatal(err)
	}

	// send and verify delivery
	lmsg := []byte("plugh")
	err = clients[1].Call(nil, "pss_sendSym", rkeyids[1], topichex, hexutil.Encode(lmsg))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case recvmsg := <-lmsgC:
		if !bytes.Equal(recvmsg.Msg, lmsg) {
			t.Fatalf("node 1 received payload mismatch: expected %v, got %v", lmsg, recvmsg)
		}
	case cerr := <-lctx.Done():
		t.Fatalf("test message timed out: %v", cerr)
	}
	rmsg := []byte("xyzzy")
	err = clients[0].Call(nil, "pss_sendSym", lkeyids[1], topichex, hexutil.Encode(rmsg))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case recvmsg := <-rmsgC:
		if !bytes.Equal(recvmsg.Msg, rmsg) {
			t.Fatalf("node 2 received payload mismatch: expected %v, got %v", rmsg, recvmsg.Msg)
		}
	case cerr := <-rctx.Done():
		t.Fatalf("test message timed out: %v", cerr)
	}
}

// send asymmetrically encrypted message between two directly connected peers
func TestAsymSend(t *testing.T) {
	t.Run("32", testAsymSend)
	t.Run("8", testAsymSend)
	t.Run("0", testAsymSend)
}

func testAsymSend(t *testing.T) {

	// address hint size
	var addrsize int64
	var err error
	paramstring := strings.Split(t.Name(), "/")
	addrsize, _ = strconv.ParseInt(paramstring[1], 10, 0)
	log.Info("asym send test", "addrsize", addrsize)

	topic := BytesToTopic([]byte("foo:42"))
	topichex := hexutil.Encode(topic[:])

	clients, err := setupNetwork(2)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Millisecond * 250)

	var loaddrhex string
	err = clients[0].Call(&loaddrhex, "pss_baseAddr")
	if err != nil {
		t.Fatalf("rpc get node 1 baseaddr fail: %v", err)
	}
	loaddr := common.FromHex(loaddrhex)
	loaddr = loaddr[:addrsize]
	var roaddrhex string
	err = clients[1].Call(&roaddrhex, "pss_baseAddr")
	if err != nil {
		t.Fatalf("rpc get node 2 baseaddr fail: %v", err)
	}
	roaddr := common.FromHex(roaddrhex)
	roaddr = roaddr[:addrsize]

	// retrieve public key from pss instance
	// set this public key reciprocally
	var lpubkey string
	err = clients[0].Call(&lpubkey, "pss_getPublicKey")
	if err != nil {
		t.Fatalf("rpc get node 1 pubkey fail: %v", err)
	}
	var rpubkey string
	err = clients[1].Call(&rpubkey, "pss_getPublicKey")
	if err != nil {
		t.Fatalf("rpc get node 2 pubkey fail: %v", err)
	}

	time.Sleep(time.Millisecond * 500) // replace with hive healthy code

	lmsgC := make(chan APIMsg)
	lctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	lsub, err := clients[0].Subscribe(lctx, "pss", lmsgC, "receive", topichex)
	log.Trace("lsub", "id", lsub)
	defer lsub.Unsubscribe()
	rmsgC := make(chan APIMsg)
	rctx, _ := context.WithTimeout(context.Background(), time.Second*10)
	rsub, err := clients[1].Subscribe(rctx, "pss", rmsgC, "receive", topichex)
	log.Trace("rsub", "id", rsub)
	defer rsub.Unsubscribe()

	// store reciprocal public keys
	err = clients[0].Call(nil, "pss_setPeerPublicKey", rpubkey, topichex, "0x")
	if err != nil {
		t.Fatal(err)
	}
	err = clients[1].Call(nil, "pss_setPeerPublicKey", lpubkey, topichex, "0x")
	if err != nil {
		t.Fatal(err)
	}

	// send and verify delivery
	rmsg := []byte("xyzzy")
	err = clients[0].Call(nil, "pss_sendAsym", rpubkey, topichex, hexutil.Encode(rmsg))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case recvmsg := <-rmsgC:
		if !bytes.Equal(recvmsg.Msg, rmsg) {
			t.Fatalf("node 2 received payload mismatch: expected %v, got %v", rmsg, recvmsg.Msg)
		}
	case cerr := <-rctx.Done():
		t.Fatalf("test message timed out: %v", cerr)
	}
	lmsg := []byte("plugh")
	err = clients[1].Call(nil, "pss_sendAsym", lpubkey, topichex, hexutil.Encode(lmsg))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case recvmsg := <-lmsgC:
		if !bytes.Equal(recvmsg.Msg, lmsg) {
			t.Fatalf("node 1 received payload mismatch: expected %v, got %v", lmsg, recvmsg.Msg)
		}
	case cerr := <-lctx.Done():
		t.Fatalf("test message timed out: %v", cerr)
	}
}

type networkParams struct {
	snapshotFile string
	numMessages  int
	addressSize  int
	adapterType  string
	messageDelay int
}

func (n *networkParams) String() string {
	return fmt.Sprintf(":%s:%d:%d:%d:%s", n.snapshotFile, n.numMessages, n.addressSize, n.messageDelay, n.adapterType)
}

// Tests random message sending in network snapshots
//
// params in run name:
// #nodes/#msgs/#addrbytes/adaptertype
// if adaptertype is exec uses execadapter, simadapter otherwise
func TestNetwork(t *testing.T) {
	var tests []*networkParams
	if *snapshotflag != "" {
		if *addresssizeflag < 0 || *addresssizeflag > 32 {
			t.Fatal("invalid address size")
		}
		_, err := os.Stat(*snapshotflag)
		if err != nil {
			t.Fatal(err)
		}
		tests = append(tests, &networkParams{
			snapshotFile: *snapshotflag,
			numMessages:  *messagesflag,
			addressSize:  *addresssizeflag,
			adapterType:  *adaptertypeflag,
			messageDelay: *messagedelayflag,
		})
	} else {
		tests = append(tests, &networkParams{
			snapshotFile: "testdata/snapshot_8.json",
			numMessages:  2,
			addressSize:  2,
			adapterType:  "sim",
			messageDelay: 1000,
		})
	}
	for _, p := range tests {
		t.Run(p.String(), testNetwork)
	}
}

func testNetwork(t *testing.T) {

	lock := &sync.Mutex{}
	type msgnotifyC struct {
		id     discover.NodeID
		msgIdx int
	}
	topic := BytesToTopic([]byte("foo:42"))
	topichex := hexutil.Encode(topic[:])

	paramstring := strings.Split(t.Name(), ":")
	msgcount, _ := strconv.ParseInt(paramstring[2], 10, 0)
	addrsize, _ := strconv.ParseInt(paramstring[3], 10, 0)
	messagedelaymax, _ := strconv.ParseInt(paramstring[4], 10, 0)
	log.Info("network test", "snapshot", paramstring[1], "msgcount", msgcount, "addrhintsize", addrsize, "messagedelay", messagedelaymax)

	sentmsgs := make([][]byte, msgcount)
	recvmsgs := make([]bool, msgcount)
	trigger := make(chan discover.NodeID)

	var adapter adapters.NodeAdapter
	if paramstring[5] == "exec" {
		dirname, err := ioutil.TempDir(".", "")
		defer os.RemoveAll(dirname)
		if err != nil {
			t.Fatal(err)
		}
		adapter = adapters.NewExecAdapter(dirname)
	} else {
		adapter = adapters.NewSimAdapter(services)
	}
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID: "0",
	})
	defer net.Shutdown()

	f, err := os.Open(paramstring[1])
	if err != nil {
		t.Fatal(err)
	}
	var snap simulations.Snapshot
	err = json.NewDecoder(f).Decode(&snap)
	if err != nil {
		t.Fatal(err)
	}
	err = net.Load(&snap)
	if err != nil {
		t.Fatal(err)
	}

	nodes := make([]discover.NodeID, len(snap.Nodes))
	bzzaddrs := make(map[discover.NodeID]string, len(snap.Nodes))
	rpcs := make(map[discover.NodeID]*rpc.Client, len(snap.Nodes))
	pubkeys := make(map[discover.NodeID]string, len(snap.Nodes))
	nodemsgcount := make(map[discover.NodeID]int, len(snap.Nodes))

	triggerChecks := func(trigger chan discover.NodeID, id discover.NodeID, rpcclient *rpc.Client) error {
		msgC := make(chan APIMsg)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		sub, err := rpcclient.Subscribe(ctx, "pss", msgC, "receive", topichex)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			defer sub.Unsubscribe()
			for {
				select {
				case recvmsg := <-msgC:
					idx, _ := binary.Uvarint(recvmsg.Msg)
					lock.Lock()
					if recvmsgs[idx] == false {
						log.Debug("msg recv", "idx", idx, "id", id)
						recvmsgs[idx] = true
						trigger <- id
					}
					lock.Unlock()
				case <-sub.Err():
					return
				}
			}
		}()
		return nil
	}

	for i, nod := range net.GetNodes() {
		nodes[i] = nod.ID()
		rpcs[nodes[i]], err = nod.Client()
		if err != nil {
			t.Fatal(err)
		}
		var pubkey string
		err = rpcs[nodes[i]].Call(&pubkey, "pss_getPublicKey")
		if err != nil {
			t.Fatal(err)
		}
		pubkeys[nod.ID()] = pubkey
		var addrhex string
		err = rpcs[nodes[i]].Call(&addrhex, "pss_baseAddr")
		if err != nil {
			t.Fatal(err)
		}
		bzzaddrs[nodes[i]] = addrhex
		err = triggerChecks(trigger, nodes[i], rpcs[nodes[i]])
		if err != nil {
			t.Fatal(err)
		}
	}

	messagedelayhigh := 0
	for i := 0; i < int(msgcount); i++ {
		sendnodeidx := rand.Intn(int(len(snap.Nodes)))
		recvnodeidx := rand.Intn(int(len(snap.Nodes) - 1))
		if recvnodeidx >= sendnodeidx {
			recvnodeidx++
		}
		nodemsgcount[nodes[recvnodeidx]]++
		sentmsgs[i] = make([]byte, 8)
		c := binary.PutUvarint(sentmsgs[i], uint64(i))
		if c == 0 {
			t.Fatal("0 byte message")
		}
		err = rpcs[nodes[sendnodeidx]].Call(nil, "pss_setPeerPublicKey", pubkeys[nodes[recvnodeidx]], topichex, bzzaddrs[nodes[recvnodeidx]])
		if err != nil {
			t.Fatal(err)
		}
		err = rpcs[nodes[recvnodeidx]].Call(nil, "pss_setPeerPublicKey", pubkeys[nodes[sendnodeidx]], topichex, bzzaddrs[nodes[sendnodeidx]])
		if err != nil {
			t.Fatal(err)
		}
		messagedelay := rand.Intn(int(messagedelaymax))
		if messagedelay > messagedelayhigh {
			messagedelayhigh = messagedelay
		}
		messagedelayduration, err := time.ParseDuration(fmt.Sprintf("%dus", messagedelay))
		if err != nil {
			t.Fatal(err)
		}
		go func(rpcclient *rpc.Client, pubkey string, msg []byte) {
			time.Sleep(messagedelayduration)
			err = rpcclient.Call(nil, "pss_sendAsym", pubkey, topichex, hexutil.Encode(msg))
			if err != nil {
				log.Error("Send asym rpc fail", "pubkey", pubkey, "topic", topichex, "err", err)
			}
		}(rpcs[nodes[sendnodeidx]], pubkeys[nodes[recvnodeidx]], sentmsgs[i])
	}

	timeout, err := time.ParseDuration(fmt.Sprintf("%dus", 60000000+messagedelayhigh))
	if err != nil {
		t.Fatal(err)
	}

	finalmsgcount := 0
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
outer:
	for i := 0; i < int(msgcount); i++ {
		select {
		case id := <-trigger:
			nodemsgcount[id]--
			finalmsgcount++
		case <-ctx.Done():
			log.Warn("timeout")
			break outer
		}
	}

	for i, msg := range recvmsgs {
		if !msg {
			log.Debug("missing message", "idx", i)
		}
	}
	t.Logf("%d of %d messages received", finalmsgcount, msgcount)

	if finalmsgcount != int(msgcount) {
		t.Fatalf("%d messages were not received", int(msgcount)-finalmsgcount)
	}

}

// symmetric send performance with varying message sizes
func BenchmarkSymkeySend(b *testing.B) {
	b.Run(fmt.Sprintf("%d", 256), benchmarkSymKeySend)
	b.Run(fmt.Sprintf("%d", 1024), benchmarkSymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024), benchmarkSymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024*10), benchmarkSymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024*100), benchmarkSymKeySend)
}

func benchmarkSymKeySend(b *testing.B) {
	msgsizestring := strings.Split(b.Name(), "/")
	if len(msgsizestring) != 2 {
		b.Fatalf("benchmark called without msgsize param")
	}
	msgsize, err := strconv.ParseInt(msgsizestring[1], 10, 0)
	if err != nil {
		b.Fatalf("benchmark called with invalid msgsize param '%s': %v", msgsizestring[1], err)
	}
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	privkey, err := w.GetPrivateKey(keys)
	ps := newTestPss(privkey, nil)
	msg := make([]byte, msgsize)
	rand.Read(msg)
	topic := BytesToTopic([]byte("foo"))
	to := make(PssAddress, 32)
	copy(to[:], network.RandomAddr().Over())
	symkeyid, err := ps.generateSymmetricKey(topic, &to, true)
	if err != nil {
		b.Fatalf("could not generate symkey: %v", err)
	}
	symkey, err := ps.w.GetSymKey(symkeyid)
	if err != nil {
		b.Fatalf("could not retreive symkey: %v", err)
	}
	ps.SetSymmetricKey(symkey, topic, &to, false)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.SendSym(symkeyid, topic, msg)
	}
}

// asymmetric send performance with varying message sizes
func BenchmarkAsymkeySend(b *testing.B) {
	b.Run(fmt.Sprintf("%d", 256), benchmarkAsymKeySend)
	b.Run(fmt.Sprintf("%d", 1024), benchmarkAsymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024), benchmarkAsymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024*10), benchmarkAsymKeySend)
	b.Run(fmt.Sprintf("%d", 1024*1024*100), benchmarkAsymKeySend)
}

func benchmarkAsymKeySend(b *testing.B) {
	msgsizestring := strings.Split(b.Name(), "/")
	if len(msgsizestring) != 2 {
		b.Fatalf("benchmark called without msgsize param")
	}
	msgsize, err := strconv.ParseInt(msgsizestring[1], 10, 0)
	if err != nil {
		b.Fatalf("benchmark called with invalid msgsize param '%s': %v", msgsizestring[1], err)
	}
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	privkey, err := w.GetPrivateKey(keys)
	ps := newTestPss(privkey, nil)
	msg := make([]byte, msgsize)
	rand.Read(msg)
	topic := BytesToTopic([]byte("foo"))
	to := make(PssAddress, 32)
	copy(to[:], network.RandomAddr().Over())
	ps.SetPeerPublicKey(&privkey.PublicKey, topic, &to)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.SendAsym(common.ToHex(crypto.FromECDSAPub(&privkey.PublicKey)), topic, msg)
	}
}
func BenchmarkSymkeyBruteforceChangeaddr(b *testing.B) {
	for i := 100; i < 100000; i = i * 10 {
		for j := 32; j < 10000; j = j * 8 {
			b.Run(fmt.Sprintf("%d/%d", i, j), benchmarkSymkeyBruteforceChangeaddr)
		}
		//b.Run(fmt.Sprintf("%d", i), benchmarkSymkeyBruteforceChangeaddr)
	}
}

// decrypt performance using symkey cache, worst case
// (decrypt key always last in cache)
func benchmarkSymkeyBruteforceChangeaddr(b *testing.B) {
	keycountstring := strings.Split(b.Name(), "/")
	cachesize := int64(0)
	var ps *Pss
	if len(keycountstring) < 2 {
		b.Fatalf("benchmark called without count param")
	}
	keycount, err := strconv.ParseInt(keycountstring[1], 10, 0)
	if err != nil {
		b.Fatalf("benchmark called with invalid count param '%s': %v", keycountstring[1], err)
	}
	if len(keycountstring) == 3 {
		cachesize, err = strconv.ParseInt(keycountstring[2], 10, 0)
		if err != nil {
			b.Fatalf("benchmark called with invalid cachesize '%s': %v", keycountstring[2], err)
		}
	}
	pssmsgs := make([]*PssMsg, 0, keycount)
	var keyid string
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	privkey, err := w.GetPrivateKey(keys)
	if cachesize > 0 {
		ps = newTestPss(privkey, &PssParams{SymKeyCacheCapacity: int(cachesize)})
	} else {
		ps = newTestPss(privkey, nil)
	}
	topic := BytesToTopic([]byte("foo"))
	for i := 0; i < int(keycount); i++ {
		to := make(PssAddress, 32)
		copy(to[:], network.RandomAddr().Over())
		keyid, err = ps.generateSymmetricKey(topic, &to, true)
		if err != nil {
			b.Fatalf("cant generate symkey #%d: %v", i, err)
		}
		symkey, err := ps.w.GetSymKey(keyid)
		if err != nil {
			b.Fatalf("could not retreive symkey %s: %v", keyid, err)
		}
		wparams := &whisper.MessageParams{
			TTL:      defaultWhisperTTL,
			KeySym:   symkey,
			Topic:    whisper.TopicType(topic),
			WorkTime: defaultWhisperWorkTime,
			PoW:      defaultWhisperPoW,
			Payload:  []byte("xyzzy"),
			Padding:  []byte("1234567890abcdef"),
		}
		woutmsg, err := whisper.NewSentMessage(wparams)
		if err != nil {
			b.Fatalf("could not create whisper message: %v", err)
		}
		env, err := woutmsg.Wrap(wparams)
		if err != nil {
			b.Fatalf("could not generate whisper envelope: %v", err)
		}
		ps.Register(&topic, func(msg []byte, p *p2p.Peer, asymmetric bool, keyid string) error {
			return nil
		})
		pssmsgs = append(pssmsgs, &PssMsg{
			To:      to,
			Payload: env,
		})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !ps.process(pssmsgs[len(pssmsgs)-(i%len(pssmsgs))-1]) {
			b.Fatalf("pss processing failed")
		}
	}
}

func BenchmarkSymkeyBruteforceSameaddr(b *testing.B) {
	for i := 100; i < 100000; i = i * 10 {
		for j := 32; j < 10000; j = j * 8 {
			b.Run(fmt.Sprintf("%d/%d", i, j), benchmarkSymkeyBruteforceSameaddr)
		}
	}
}

// decrypt performance using symkey cache, best case
// (decrypt key always first in cache)
func benchmarkSymkeyBruteforceSameaddr(b *testing.B) {
	var keyid string
	var ps *Pss
	cachesize := int64(0)
	keycountstring := strings.Split(b.Name(), "/")
	if len(keycountstring) < 2 {
		b.Fatalf("benchmark called without count param")
	}
	keycount, err := strconv.ParseInt(keycountstring[1], 10, 0)
	if err != nil {
		b.Fatalf("benchmark called with invalid count param '%s': %v", keycountstring[1], err)
	}
	if len(keycountstring) == 3 {
		cachesize, err = strconv.ParseInt(keycountstring[2], 10, 0)
		if err != nil {
			b.Fatalf("benchmark called with invalid cachesize '%s': %v", keycountstring[2], err)
		}
	}
	addr := make([]PssAddress, keycount)
	ctx, _ := context.WithTimeout(context.Background(), time.Second)
	keys, err := wapi.NewKeyPair(ctx)
	privkey, err := w.GetPrivateKey(keys)
	if cachesize > 0 {
		ps = newTestPss(privkey, &PssParams{SymKeyCacheCapacity: int(cachesize)})
	} else {
		ps = newTestPss(privkey, nil)
	}
	topic := BytesToTopic([]byte("foo"))
	for i := 0; i < int(keycount); i++ {
		copy(addr[i], network.RandomAddr().Over())
		keyid, err = ps.generateSymmetricKey(topic, &addr[i], true)
		if err != nil {
			b.Fatalf("cant generate symkey #%d: %v", i, err)
		}

	}
	symkey, err := ps.w.GetSymKey(keyid)
	if err != nil {
		b.Fatalf("could not retreive symkey %s: %v", keyid, err)
	}
	wparams := &whisper.MessageParams{
		TTL:      defaultWhisperTTL,
		KeySym:   symkey,
		Topic:    whisper.TopicType(topic),
		WorkTime: defaultWhisperWorkTime,
		PoW:      defaultWhisperPoW,
		Payload:  []byte("xyzzy"),
		Padding:  []byte("1234567890abcdef"),
	}
	woutmsg, err := whisper.NewSentMessage(wparams)
	if err != nil {
		b.Fatalf("could not create whisper message: %v", err)
	}
	env, err := woutmsg.Wrap(wparams)
	if err != nil {
		b.Fatalf("could not generate whisper envelope: %v", err)
	}
	ps.Register(&topic, func(msg []byte, p *p2p.Peer, asymmetric bool, keyid string) error {
		return nil
	})
	pssmsg := &PssMsg{
		To:      addr[len(addr)-1][:],
		Payload: env,
	}
	for i := 0; i < b.N; i++ {
		if !ps.process(pssmsg) {
			b.Fatalf("pss processing failed: %v", err)
		}
	}
}

// setup simulated network and connect nodes in circle
func setupNetwork(numnodes int) (clients []*rpc.Client, err error) {
	nodes := make([]*simulations.Node, numnodes)
	clients = make([]*rpc.Client, numnodes)
	if numnodes < 2 {
		return nil, fmt.Errorf("Minimum two nodes in network")
	}
	adapter := adapters.NewSimAdapter(services)
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID:             "0",
		DefaultService: "bzz",
	})
	for i := 0; i < numnodes; i++ {
		nodes[i], err = net.NewNodeWithConfig(&adapters.NodeConfig{
			Services: []string{"bzz", "pss"},
		})
		if err != nil {
			return nil, fmt.Errorf("error creating node 1: %v", err)
		}
		err = net.Start(nodes[i].ID())
		if err != nil {
			return nil, fmt.Errorf("error starting node 1: %v", err)
		}
		if i > 0 {
			err = net.Connect(nodes[i].ID(), nodes[i-1].ID())
			if err != nil {
				return nil, fmt.Errorf("error connecting nodes: %v", err)
			}
		}
		clients[i], err = nodes[i].Client()
		if err != nil {
			return nil, fmt.Errorf("create node 1 rpc client fail: %v", err)
		}
	}
	if numnodes > 2 {
		err = net.Connect(nodes[0].ID(), nodes[len(nodes)-1].ID())
		if err != nil {
			return nil, fmt.Errorf("error connecting first and last nodes")
		}
	}
	return clients, nil
}

func newServices() adapters.Services {
	stateStore := newStateStore()
	kademlias := make(map[discover.NodeID]*network.Kademlia)
	kademlia := func(id discover.NodeID) *network.Kademlia {
		if k, ok := kademlias[id]; ok {
			return k
		}
		addr := network.NewAddrFromNodeID(id)
		params := network.NewKadParams()
		params.MinProxBinSize = 2
		params.MaxBinSize = 3
		params.MinBinSize = 1
		params.MaxRetries = 1000
		params.RetryExponent = 2
		params.RetryInterval = 1000000
		kademlias[id] = network.NewKademlia(addr.Over(), params)
		return kademlias[id]
	}
	return adapters.Services{
		"pss": func(ctx *adapters.ServiceContext) (node.Service, error) {
			cachedir, err := ioutil.TempDir("", "pss-cache")
			if err != nil {
				return nil, fmt.Errorf("create pss cache tmpdir failed", "error", err)
			}
			dpa, err := storage.NewLocalDPA(cachedir)
			if err != nil {
				return nil, fmt.Errorf("local dpa creation failed", "error", err)
			}

			ctxlocal, _ := context.WithTimeout(context.Background(), time.Second)
			keys, err := wapi.NewKeyPair(ctxlocal)
			privkey, err := w.GetPrivateKey(keys)
			pssp := NewPssParams(privkey)
			pssp.MsgTTL = time.Second * 30
			pskad := kademlia(ctx.Config.ID)
			ps := NewPss(pskad, dpa, pssp)

			ping := &Ping{
				OutC: make(chan bool),
				Pong: true,
			}
			p2pp := NewPingProtocol(ping)
			pp, err := RegisterProtocol(ps, &PingTopic, PingProtocol, p2pp, &ProtocolParams{Asymmetric: true})
			if err != nil {
				return nil, err
			}
			if useHandshake {
				SetHandshakeController(ps, NewHandshakeParams())
			}
			ps.Register(&PingTopic, pp.Handle)
			ps.addAPI(rpc.API{
				Namespace: "psstest",
				Version:   "0.2",
				Service:   NewAPITest(ps),
				Public:    false,
			})
			if err != nil {
				log.Error("Couldnt register pss protocol", "err", err)
				os.Exit(1)
			}
			pssprotocols[ctx.Config.ID.String()] = &protoCtrl{
				C:        ping.OutC,
				protocol: pp,
				run:      p2pp.Run,
			}
			return ps, nil
		},
		"bzz": func(ctx *adapters.ServiceContext) (node.Service, error) {
			addr := network.NewAddrFromNodeID(ctx.Config.ID)
			hp := network.NewHiveParams()
			hp.Discovery = false
			config := &network.BzzConfig{
				OverlayAddr:  addr.Over(),
				UnderlayAddr: addr.Under(),
				HiveParams:   hp,
			}
			return network.NewBzz(config, kademlia(ctx.Config.ID), stateStore), nil
		},
	}
}

func newTestPss(privkey *ecdsa.PrivateKey, ppextra *PssParams) *Pss {

	var nid discover.NodeID
	copy(nid[:], crypto.FromECDSAPub(&privkey.PublicKey))
	addr := network.NewAddrFromNodeID(nid)

	// set up storage
	cachedir, err := ioutil.TempDir("", "pss-cache")
	if err != nil {
		log.Error("create pss cache tmpdir failed", "error", err)
		os.Exit(1)
	}
	dpa, err := storage.NewLocalDPA(cachedir)
	if err != nil {
		log.Error("local dpa creation failed", "error", err)
		os.Exit(1)
	}

	// set up routing
	kp := network.NewKadParams()
	kp.MinProxBinSize = 3

	// create pss
	pp := NewPssParams(privkey)
	if ppextra != nil {
		pp.SymKeyCacheCapacity = ppextra.SymKeyCacheCapacity
	}

	overlay := network.NewKademlia(addr.Over(), kp)
	ps := NewPss(overlay, dpa, pp)

	return ps
}

// API calls for test/development use
type APITest struct {
	*Pss
}

func NewAPITest(ps *Pss) *APITest {
	return &APITest{Pss: ps}
}

func (apitest *APITest) SetSymKeys(pubkeyid string, recvsymkey []byte, sendsymkey []byte, limit uint16, topicbytes hexutil.Bytes, to hexutil.Bytes) ([2]string, error) {
	var topic Topic
	copy(topic[:], topicbytes)
	addr := make(PssAddress, len(to))
	copy(addr[:], to)
	recvsymkeyid, err := apitest.SetSymmetricKey(recvsymkey, topic, &addr, true)
	if err != nil {
		return [2]string{}, err
	}
	sendsymkeyid, err := apitest.SetSymmetricKey(sendsymkey, topic, &addr, false)
	if err != nil {
		return [2]string{}, err
	}
	return [2]string{recvsymkeyid, sendsymkeyid}, nil
}

func (apitest *APITest) Clean() (int, error) {
	return apitest.Pss.cleanKeys(), nil
}
