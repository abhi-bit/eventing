package util

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"math"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/couchbase/cbauth"
	"github.com/couchbase/cbauth/metakv"
	cm "github.com/couchbase/eventing/common"
	mcd "github.com/couchbase/eventing/dcp/transport"
	"github.com/couchbase/eventing/logging"
	"github.com/couchbase/gocb"
	"github.com/couchbase/gomemcached"
)

const (
	EventingAdminService = "eventingAdminPort"
	DataService          = "kv"
	MgmtService          = "mgmt"

	HTTPRequestTimeout = time.Duration(5000) * time.Millisecond

	EPSILON = 1e-5
)

const (
	metakvMaxDocSize = 4096 //Fragment size for Appcontent
)

var GocbCredsRequestCounter = 0

type Uint16Slice []uint16

func (s Uint16Slice) Len() int           { return len(s) }
func (s Uint16Slice) Less(i, j int) bool { return s[i] < s[j] }
func (s Uint16Slice) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type Config map[string]interface{}

type ConfigHolder struct {
	ptr unsafe.Pointer
}

type DynamicAuthenticator struct {
	Caller string
}

func (h *ConfigHolder) Store(conf Config) {
	atomic.StorePointer(&h.ptr, unsafe.Pointer(&conf))
}

func (h *ConfigHolder) Load() Config {
	confPtr := atomic.LoadPointer(&h.ptr)
	return *(*Config)(confPtr)
}

func NewConfig(data interface{}) (Config, error) {
	config := make(Config)
	err := config.Update(data)
	return config, err
}

func (config Config) Update(data interface{}) error {
	switch v := data.(type) {
	case Config:
		for key, value := range v {
			config.Set(key, value)
		}
	default:
	}
	return nil
}

func (config Config) Set(key string, value interface{}) Config {
	config[key] = value
	return config
}

func ComputeMD5(data []byte) ([]byte, error) {
	hash := md5.New()
	if _, err := io.Copy(hash, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	checksum := hash.Sum(nil)
	return checksum, nil
}

type PayloadHash struct {
	Fragmentcnt  int      `json:"fragment_count"`
	Fragmenthash [][]byte `json:"fragment_hash"`
}

func (payloadhash *PayloadHash) Update(payload []byte, fragmentSize int) error {
	length := len(payload)
	fragmentCount := length / fragmentSize
	if length%fragmentSize != 0 {
		fragmentCount++
	}
	payloadhash.Fragmentcnt = fragmentCount
	payloadhash.Fragmenthash = make([][]byte, fragmentCount)
	for idx := 0; idx < fragmentCount; idx++ {
		curridx := idx * fragmentSize
		lastidx := (idx + 1) * fragmentSize
		if lastidx > length {
			lastidx = length
		}
		checksum, err := ComputeMD5(payload[curridx:lastidx])
		if err != nil {
			return err
		}
		payloadhash.Fragmenthash[idx] = checksum
	}
	return nil
}

func SprintDCPCounts(counts map[mcd.CommandCode]uint64) (string, uint64, time.Time) {
	line := ""
	ops := uint64(0)
	currTimestamp := time.Now()

	for i := 0; i < 256; i++ {
		opcode := mcd.CommandCode(i)
		if n, ok := counts[opcode]; ok {
			line += fmt.Sprintf("%s:%v ", mcd.CommandNames[opcode], n)
			ops += n
		}
	}
	return strings.TrimRight(line, " "), ops, currTimestamp
}

func SprintV8Counts(counts map[string]uint64) string {
	line := ""
	for k, v := range counts {
		line += fmt.Sprintf("%s:%v ", k, v)
	}
	return strings.TrimRight(line, " ")
}

func NsServerNodesAddresses(auth, hostaddress string) ([]string, error) {
	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return nil, err
	}

	nsServerAddrs := cinfo.GetNodesByServiceType(MgmtService)

	nsServerNodes := []string{}
	for _, nsServerAddr := range nsServerAddrs {
		addr, _ := cinfo.GetServiceAddress(nsServerAddr, MgmtService)
		nsServerNodes = append(nsServerNodes, addr)
	}

	sort.Strings(nsServerNodes)

	return nsServerNodes, nil
}

func KVNodesAddresses(auth, hostaddress string) ([]string, error) {
	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return nil, err
	}

	kvAddrs := cinfo.GetNodesByServiceType(DataService)

	kvNodes := []string{}
	for _, kvAddr := range kvAddrs {
		addr, _ := cinfo.GetServiceAddress(kvAddr, DataService)
		kvNodes = append(kvNodes, addr)
	}

	sort.Strings(kvNodes)

	return kvNodes, nil
}

func EventingNodesAddresses(auth, hostaddress string) ([]string, error) {
	logPrefix := "util::EventingNodesAddresses"

	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return nil, err
	}

	eventingAddrs := cinfo.GetNodesByServiceType(EventingAdminService)

	eventingNodes := []string{}
	for _, eventingAddr := range eventingAddrs {
		addr, err := cinfo.GetServiceAddress(eventingAddr, EventingAdminService)
		if err != nil {
			logging.Errorf("%s Failed to get eventing node address, err: %v", logPrefix, err)
			continue
		}
		eventingNodes = append(eventingNodes, addr)
	}

	sort.Strings(eventingNodes)

	return eventingNodes, nil
}

func CurrentEventingNodeAddress(auth, hostaddress string) (string, error) {
	logPrefix := "util::CurrentEventingNodeAddress"

	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return "", err
	}

	cNodeID := cinfo.GetCurrentNode()
	eventingNode, err := cinfo.GetServiceAddress(cNodeID, EventingAdminService)
	if err != nil {
		logging.Errorf("%s Failed to get current eventing node address, err: %v", logPrefix, err)
		return "", err
	}
	return eventingNode, nil
}

func LocalEventingServiceHost(auth, hostaddress string) (string, error) {
	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return "", err
	}

	srvAddr, err := cinfo.GetLocalServiceHost(EventingAdminService)
	if err != nil {
		return "", err
	}

	return srvAddr, nil
}

func KVVbMap(auth, bucket, hostaddress string) (map[uint16]string, error) {
	logPrefix := "util::KVVbMap"

	cinfo, err := FetchNewClusterInfoCache(hostaddress)
	if err != nil {
		return nil, err
	}

	kvAddrs := cinfo.GetNodesByServiceType(DataService)

	kvVbMap := make(map[uint16]string)

	for _, kvAddr := range kvAddrs {
		addr, err := cinfo.GetServiceAddress(kvAddr, DataService)
		if err != nil {
			logging.Errorf("%s Failed to get address of KV host: %rs, err: %v", logPrefix, kvAddr, err)
			return nil, err
		}

		vbs, err := cinfo.GetVBuckets(kvAddr, bucket)
		if err != nil {
			logging.Errorf("%s Failed to get vbuckets for given kv: %rs, err: %v", logPrefix, kvAddr, err)
			continue
		}

		for i := 0; i < len(vbs); i++ {
			kvVbMap[uint16(vbs[i])] = addr
		}
	}

	return kvVbMap, nil
}

// Write to the admin console
func Console(clusterAddr string, format string, v ...interface{}) error {
	msg := fmt.Sprintf(format, v...)
	values := url.Values{"message": {msg}, "logLevel": {"info"}, "component": {"indexing"}}

	if !strings.HasPrefix(clusterAddr, "http://") {
		clusterAddr = "http://" + clusterAddr
	}
	clusterAddr += "/_log"

	_, err := PostForm(clusterAddr, values)

	return err
}

func StopDebugger(urlSuffix, nodeAddr, appName string) {
	endpointURL := fmt.Sprintf("http://%s/stopDebugger/?name=%s", nodeAddr, appName)
	netClient := NewClient(HTTPRequestTimeout)

	_, err := netClient.Get(endpointURL)
	if err != nil {
		logging.Errorf("UTIL Failed to capture v8 debugger url from url: %rs, err: %v", endpointURL, err)
		return
	}
	return
}

func GetDebuggerURL(urlSuffix, nodeAddr, appName string) string {
	logPrefix := "util::GetDebuggerURL"

	if nodeAddr == "" {
		logging.Verbosef("%s Debugger host not found. Debugger not started", logPrefix)
		return ""
	}

	endpointURL := fmt.Sprintf("http://%s/%s/?name=%s", nodeAddr, urlSuffix, appName)

	netClient := NewClient(HTTPRequestTimeout)

	res, err := netClient.Get(endpointURL)
	if err != nil {
		logging.Errorf("%s Failed to capture v8 debugger url from url: %rs, err: %v", logPrefix, endpointURL, err)
		return ""
	}

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		logging.Errorf("%s Failed to read v8 debugger url response from url: %rs, err: %v", logPrefix, endpointURL, err)
		return ""
	}

	return string(buf)
}

func GetNodeUUIDs(urlSuffix string, nodeAddrs []string) (map[string]string, error) {
	logPrefix := "util::GetNodeUUIDs"

	addrUUIDMap := make(map[string]string)

	netClient := NewClient(HTTPRequestTimeout)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to fetch node uuid from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}

		addrUUIDMap[string(buf)] = nodeAddr
	}
	return addrUUIDMap, nil
}

func GetEventProcessingStats(urlSuffix string, nodeAddrs []string) (map[string]int64, error) {
	logPrefix := "util::GetEventProcessingStats"

	pStats := make(map[string]int64)

	netClient := NewClient(HTTPRequestTimeout)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to gather event processing stats from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body for event processing stats from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}

		var nodePStats map[string]int64
		err = json.Unmarshal(buf, &nodePStats)
		if err != nil {
			logging.Errorf("%s Failed to unmarshal event processing stats from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}

		for k, v := range nodePStats {
			if _, ok := pStats[k]; !ok {
				pStats[k] = 0
			}
			pStats[k] += v
		}
	}

	return pStats, nil
}

func GetProgress(urlSuffix string, nodeAddrs []string) (*cm.RebalanceProgress, map[string]error) {
	logPrefix := "util::GetProgress"

	aggProgress := &cm.RebalanceProgress{}

	netClient := NewClient(HTTPRequestTimeout)

	errMap := make(map[string]error)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to gather task status from url: %rs, err: %v", logPrefix, endpointURL, err)
			errMap[nodeAddr] = err
			continue
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body from url: %rs, err: %v", logPrefix, endpointURL, err)
			errMap[nodeAddr] = err
			continue
		}

		var progress cm.RebalanceProgress
		err = json.Unmarshal(buf, &progress)
		if err != nil {
			logging.Warnf("%s Failed to unmarshal progress from url: %rs, err: %v", logPrefix, endpointURL, err)
			errMap[nodeAddr] = err
			continue
		}

		logging.Infof("%s endpointURL: %rs VbsRemainingToShuffle: %d VbsOwnedPerPlan: %d",
			logPrefix, endpointURL, progress.VbsRemainingToShuffle, progress.VbsOwnedPerPlan)

		aggProgress.VbsRemainingToShuffle += progress.VbsRemainingToShuffle
		aggProgress.VbsOwnedPerPlan += progress.VbsOwnedPerPlan
	}

	return aggProgress, errMap
}

func GetDeployedApps(urlSuffix string, nodeAddrs []string) (map[string]map[string]string, error) {
	logPrefix := "util::GetDeployedApps"

	deployedApps := make(map[string]map[string]string)

	netClient := NewClient(HTTPRequestTimeout)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to get deployed apps from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}

		var locallyDeployedApps map[string]string
		err = json.Unmarshal(buf, &locallyDeployedApps)
		if err != nil {
			logging.Errorf("%s Failed to unmarshal deployed apps from url: %rs, err: %v", logPrefix, endpointURL, err)
			return nil, err
		}

		deployedApps[nodeAddr] = make(map[string]string)
		deployedApps[nodeAddr] = locallyDeployedApps
	}

	return deployedApps, nil
}

func ListChildren(path string) []string {
	logPrefix := "util::ListChildren"

	entries, err := metakv.ListAllChildren(path)
	if err != nil {
		logging.Errorf("%s Failed to fetch deployed app list from metakv, err: %v", logPrefix, err)
		return nil
	}

	var children []string
	hmap := make(map[string]bool)
	for _, entry := range entries {
		splitRes := strings.Split(entry.Path, "/")
		child := splitRes[len(splitRes)-2]
		if _, seen := hmap[child]; seen == false {
			hmap[child] = true
			children = append(children, child)
		}
	}
	return children
}

func MetakvGet(path string) ([]byte, error) {
	data, _, err := metakv.Get(path)
	if err != nil {
		return nil, err
	}
	return data, err
}

func MetakvSet(path string, value []byte, rev interface{}) error {
	return metakv.Set(path, value, rev)
}

func MetaKvDelete(path string, rev interface{}) error {
	return metakv.Delete(path, rev)
}

func MetakvRecursiveDelete(dirpath string) error {
	return metakv.RecursiveDelete(dirpath)
}

func RecursiveDelete(dirpath string) error {
	return metakv.RecursiveDelete(dirpath)
}

//WriteAppContent fragments the payload and store it to metakv
func WriteAppContent(appsPath, checksumPath, appName string, payload []byte) error {
	logPrefix := "util::WriteAppContent"
	appsPath += appName
	appsPath += "/"
	length := len(payload)

	checksumPath += appName
	fragmentCount := length / metakvMaxDocSize
	if length%metakvMaxDocSize != 0 {
		fragmentCount++
	}
	logging.Infof("%s Number of fragments: %d payload size: %d appName: %s", logPrefix, fragmentCount, length, appName)
	for idx := 0; idx < fragmentCount; idx++ {
		currpath := appsPath + strconv.Itoa(int(idx))
		curridx := idx * metakvMaxDocSize
		lastidx := (idx + 1) * metakvMaxDocSize
		if lastidx > length {
			lastidx = length
		}
		fragment := payload[curridx:lastidx]
		err := MetakvSet(currpath, fragment, nil)
		if err != nil {
			//Delete existing entry from appspath
			logging.Errorf("%s MetakvSet failed for fragments, fragment number: %d appName: %s err: %v", logPrefix, idx, appName, err)
			if errd := MetakvRecursiveDelete(appsPath); errd != nil {
				logging.Errorf("%s MetakvSet::MetakvRecursiveDelete failed, fragment number: %d appName: %s err: %v", logPrefix, idx, appName, errd)
				return errd
			}
			return err
		}
	}

	//Compute MD5 hash and Update it to metakv
	payloadhash := PayloadHash{}
	if err := payloadhash.Update(payload, metakvMaxDocSize); err != nil {
		logging.Errorf("%s Updating payload hash failed, appName: %s err: %v", logPrefix, appName, err)
		//Delete existing entry from appspath
		if errd := MetakvRecursiveDelete(appsPath); errd != nil {
			logging.Errorf("%s Payloadhash::MetakvRecursiveDelete failed, appName: %s err: %v", logPrefix, appName, errd)
			return errd
		}
		return err
	}

	//Marshal payloadhash and update it to metakv
	hashdata, err := json.Marshal(&payloadhash)
	if err != nil {
		//Delete existing entry from appspath
		logging.Errorf("%s Json Marshal failed appName: %s err: %v", logPrefix, appName, err)
		if errd := MetakvRecursiveDelete(appsPath); errd != nil {
			logging.Errorf("%s : JsonMarshal::MetaRecursiveDelete appName: %s err: %v", logPrefix, appName, errd)
			return errd
		}
		return err
	}

	if err = MetakvSet(checksumPath, hashdata, nil); err != nil {
		//Delete existing entry from appspath
		logging.Errorf("%s MetakvSet failed for checksum appName: %s err: %v", logPrefix, appName, err)
		if errd := MetakvRecursiveDelete(appsPath); errd != nil {
			logging.Errorf("%s Checksum::MetakvRecursiveDelete appName: %s err: %v", logPrefix, appName, errd)
			return errd
		}
		return err
	}

	return nil
}

//ReadAppContent reads Handler Code
func ReadAppContent(appsPath, checksumPath, appName string) ([]byte, error) {
	//Fetch Checksum data from metakv and unmarshal it
	logPrefix := "util::ReadAppContent"
	checksumPath += appName
	var payloadhash PayloadHash
	if hashdata, err := MetakvGet(checksumPath); err != nil {
		logging.Errorf("%s MetakvGet failed for checksum appName: %s err: %v", logPrefix, appName, err)
		return nil, err
	} else {
		if err := json.Unmarshal(hashdata, &payloadhash); err != nil {
			logging.Errorf("%s Json Unmarshal failed for checksum appName: %s", logPrefix, appName)
			return nil, err
		}
	}

	//Read fragment data
	var payload []byte
	appsPath += appName
	for idx := 0; idx < payloadhash.Fragmentcnt; idx++ {
		path := appsPath + "/" + strconv.Itoa(int(idx))
		data, err := MetakvGet(path)
		if err != nil {
			logging.Errorf("%s MetakvGet failed for fragments, fragment number: %d fragment count: %d appName: %s err: %v", logPrefix, idx, payloadhash.Fragmentcnt, appName, err)
			return nil, err
		}

		if data == nil {
			logging.Errorf("%s MetakvGet data is empty,  fragment number: %d fragment count: %d appName: %s", logPrefix, idx, payloadhash.Fragmentcnt, appName)
			return nil, errors.New("Reading stale data")
		}

		if fragmenthash, err := ComputeMD5(data); err != nil {
			logging.Errorf("%s MetakvGet MD5 computation failed, fragment number: %d fragment count: %d appName: %s err: %v", logPrefix, idx, payloadhash.Fragmentcnt, appName, err)
			return nil, err
		} else {
			if bytes.Equal(fragmenthash, payloadhash.Fragmenthash[idx]) != true {
				logging.Errorf("%s MetakvGet Checksum Mismatch, fragment number: %d fragment count: %d appName: %s", logPrefix, idx, payloadhash.Fragmentcnt, appName)
				return nil, errors.New("Checksum mismatch for payload fragments")
			}
			payload = append(payload, data...)
		}
	}
	return payload, nil
}

//DeleteAppContent delete handler code
func DeleteAppContent(appPath, checksumPath, appName string) error {
	//Delete Checksum path
	logPrefix := "util::DeleteAppContent"
	checksumPath += appName
	if err := MetaKvDelete(checksumPath, nil); err != nil {
		logging.Errorf("%s MetakvDelete failed for checksum appName: %s err: %v", logPrefix, appName, err)
		return err
	}

	//Delete Apps Path
	appPath += appName
	appPath += "/"
	if err := MetakvRecursiveDelete(appPath); err != nil {
		logging.Errorf("%s MetakvRecursiveDelete failed for Apps appName: %s err: %v", logPrefix, appName, err)
		return err
	}
	return nil
}

//Delete stale app fragments
func DeleteStaleAppContent(appPath, appName string) error {
	//Delete Apps Path
	logPrefix := "util::DeleteStaleAppContent"
	appPath += appName
	appPath += "/"
	if err := MetakvRecursiveDelete(appPath); err != nil {
		logging.Errorf("%s MetakvRecursiveDelete failed for Apps appName: %s err: %v", logPrefix, appName, err)
		return err
	}
	return nil
}

func GetHash(appCode string) string {
	hash := md5.New()
	hash.Write([]byte(appCode))
	return fmt.Sprintf("%d-%x", len(appCode), hash.Sum(nil))
}

func MemcachedErrCode(err error) gomemcached.Status {
	status := gomemcached.Status(0xffff)
	if res, ok := err.(*gomemcached.MCResponse); ok {
		status = res.Status
	}
	return status
}

func CompareSlices(s1, s2 []string) bool {

	if s1 == nil && s2 == nil {
		return true
	}

	if s1 == nil || s2 == nil {
		return false
	}

	if len(s1) != len(s2) {
		return false
	}

	for i := range s1 {
		if s1[i] != s2[i] {
			return false
		}
	}

	return true
}

func VbsSliceDiff(X, Y []uint16) []uint16 {
	var diff []uint16

	m := make(map[uint16]int)

	for _, y := range Y {
		m[y]++
	}

	for _, x := range X {
		if m[x] > 0 {
			m[x]--
			continue
		}
		diff = append(diff, x)
	}

	n := make(map[uint16]int)

	for _, x := range X {
		n[x]++
	}

	for _, y := range Y {
		if n[y] > 0 {
			n[y]--
			continue
		}
		diff = append(diff, y)
	}

	return diff
}

func Uint16SliceDiff(kv1, kv2 []uint16) []uint16 {
	var diff []uint16

	for _, s1 := range kv1 {
		found := false
		for _, s2 := range kv2 {
			if s1 == s2 {
				found = true
				break
			}
		}

		if !found {
			diff = append(diff, s1)
		}
	}

	return diff
}

func StrSliceDiff(kv1, kv2 []string) []string {
	var diff []string

	for _, s1 := range kv1 {
		found := false
		for _, s2 := range kv2 {
			if s1 == s2 {
				found = true
				break
			}
		}

		if !found {
			diff = append(diff, s1)
		}
	}

	return diff
}

func ConvertBigEndianToUint64(cas []byte) (uint64, error) {
	if len(cas) == 0 {
		return 0, fmt.Errorf("empty cas value")
	}

	// Trim "Ox"
	cas = cas[2:]

	for i := 0; i < len(cas)/2; i += 2 {
		cas[i], cas[len(cas)-i-2] = cas[len(cas)-i-2], cas[i]
		cas[i+1], cas[len(cas)-i-1] = cas[len(cas)-i-1], cas[i+1]
	}

	return strconv.ParseUint(string(cas), 16, 64)
}

func GetLogLevel(logLevel string) logging.LogLevel {
	switch logLevel {
	case "ERROR":
		return logging.Error
	case "INFO":
		return logging.Info
	case "WARNING":
		return logging.Warn
	case "DEBUG":
		return logging.Debug
	case "TRACE":
		return logging.Trace
	default:
		return logging.Info
	}
}

// VbucketNodeAssignment will be used as generic partitioning scheme for vbucket assignment to
// Eventing.Consumer and Eventing.Producer instances
func VbucketNodeAssignment(vbs []uint16, numWorkers int) map[int][]uint16 {
	vbucketsPerWorker := len(vbs) / numWorkers

	var vbNo int

	vbWorkerAssignMap := make(map[int][]uint16)
	if len(vbs) == 0 {
		for i := 0; i < numWorkers; i++ {
			assignedVbs := make([]uint16, 0)
			vbWorkerAssignMap[i] = assignedVbs
		}

		return vbWorkerAssignMap
	}

	vbCountPerWorker := make([]int, numWorkers)
	for i := 0; i < numWorkers; i++ {
		vbCountPerWorker[i] = vbucketsPerWorker
		vbNo += vbucketsPerWorker
	}

	remainingVbs := len(vbs) - vbNo
	if remainingVbs > 0 {
		for i := 0; i < remainingVbs; i++ {
			vbCountPerWorker[i] = vbCountPerWorker[i] + 1
		}
	}

	startVb := vbs[0]
	for i, v := range vbCountPerWorker {
		assignedVbs := make([]uint16, 0)
		for j := 0; j < v; j++ {
			assignedVbs = append(assignedVbs, startVb)
			startVb++
		}
		vbWorkerAssignMap[i] = assignedVbs
	}

	return vbWorkerAssignMap
}

// VbucketDistribution is used by vbucket ownership give up and takeover routines during rebalance
func VbucketDistribution(vbs []uint16, numWorkers int) map[int][]uint16 {
	vbWorkerAssignMap := make(map[int][]uint16)
	for i := 0; i < numWorkers; i++ {
		assignedVbs := make([]uint16, 0)
		vbWorkerAssignMap[i] = assignedVbs
	}

	if len(vbs) == 0 || numWorkers == 0 {
		return vbWorkerAssignMap
	}

	for i := 0; i < len(vbs); {
		for j := 0; j < numWorkers; j++ {
			if i < len(vbs) {
				vbWorkerAssignMap[j] = append(vbWorkerAssignMap[j], vbs[i])
				i++
			} else {
				return vbWorkerAssignMap
			}
		}
	}

	return vbWorkerAssignMap
}

func Condense(vbs []uint16) string {
	if len(vbs) == 0 {
		return "[]"
	}

	startVb := vbs[0]
	res := fmt.Sprintf("[%d", startVb)
	prevVb := startVb

	for i := 1; i < len(vbs); {
		if vbs[i] == startVb+1 {
			startVb++
		} else {

			if prevVb != startVb {
				res = fmt.Sprintf("%s-%d, %d", res, startVb, vbs[i])
			} else {
				res = fmt.Sprintf("%s, %d", res, vbs[i])
			}
			startVb = vbs[i]
			prevVb = startVb
		}

		if i == len(vbs)-1 {
			if prevVb == vbs[i] {
				res = fmt.Sprintf("%s]", res)
				return res
			}

			res = fmt.Sprintf("%s-%d]", res, vbs[i])
			return res
		}

		i++
	}

	return res
}

// VbucketByKey returns doc_id to vbucket mapping
func VbucketByKey(key []byte, numVbuckets int) uint16 {
	return uint16((crc32.ChecksumIEEE(key) >> 16) % uint32(numVbuckets))
}

func StripScheme(endpoint string) string {
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
}

func (dynAuth *DynamicAuthenticator) Credentials(req gocb.AuthCredsRequest) ([]gocb.UserPassPair, error) {
	logPrefix := "DynamicAuthenticator::Credentials"

	GocbCredsRequestCounter++
	strippedEndpoint := StripScheme(req.Endpoint)
	username, password, err := cbauth.GetMemcachedServiceAuth(strippedEndpoint)
	if err != nil {
		logging.Errorf("%s invoked by %s, failed to get auth from cbauth", logPrefix, dynAuth.Caller)
		return []gocb.UserPassPair{{}}, err
	}

	return []gocb.UserPassPair{{
		Username: username,
		Password: password,
	}}, nil
}

func CheckIfRebalanceOngoing(urlSuffix string, nodeAddrs []string) (bool, error) {
	logPrefix := "util::CheckIfRebalanceOngoing"

	netClient := NewClient(HTTPRequestTimeout)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to gather rebalance status from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}

		status, err := strconv.ParseBool(string(buf))
		if err != nil {
			logging.Errorf("%s Failed to interpret rebalance status from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}

		logging.Infof("%s Rebalance status from url: %rs status: %v", logPrefix, endpointURL, status)

		if status {
			return true, nil
		}

	}

	return false, nil
}

func GetAggBootstrappingApps(urlSuffix string, nodeAddrs []string) (bool, error) {
	logPrefix := "util::GetAggBootstrappingApps"

	netClient := NewClient(HTTPRequestTimeout)

	for _, nodeAddr := range nodeAddrs {
		endpointURL := fmt.Sprintf("http://%s%s", nodeAddr, urlSuffix)

		res, err := netClient.Get(endpointURL)
		if err != nil {
			logging.Errorf("%s Failed to gather bootstrapping app list from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}
		defer res.Body.Close()

		buf, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logging.Errorf("%s Failed to read response body from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}

		bootstrappingApps := make(map[string]string)
		err = json.Unmarshal(buf, &bootstrappingApps)
		if err != nil {
			logging.Errorf("%s Failed to marshal bootstrapping app list from url: %rs, err: %v", logPrefix, endpointURL, err)
			return true, err
		}

		if len(bootstrappingApps) > 0 {
			return true, fmt.Errorf("Some apps are undergoing bootstrap")
		}
	}

	return false, nil
}

func Contains(needle interface{}, haystack interface{}) bool {
	s := reflect.ValueOf(haystack)

	if s.Kind() != reflect.Slice {
		panic("non-slice type provided")
	}

	for i := 0; i < s.Len(); i++ {
		if s.Index(i).Interface() == needle {
			return true
		}
	}

	return false
}

func ContainsIgnoreCase(needle string, haystack []string) bool {
	for _, item := range haystack {
		if strings.EqualFold(item, needle) {
			return true
		}
	}

	return false
}

func ToStr(value bool) (strValue string) {
	if value {
		strValue = "1"
	} else {
		strValue = "0"
	}

	return
}

func ToStringArray(from interface{}) (to []string) {
	if from == nil {
		return
	}

	fromArray := from.([]interface{})
	to = make([]string, len(fromArray))
	for i := 0; i < len(fromArray); i++ {
		to[i] = fromArray[i].(string)
	}

	return
}

func FloatEquals(a, b float64) bool {
	return math.Abs(a-b) <= EPSILON
}

func DeepCopy(kv map[string]interface{}) (newKv map[string]interface{}) {
	newKv = make(map[string]interface{})
	for k, v := range kv {
		newKv[k] = v
	}

	return
}

func GetAppNameFromPath(path string) string {
	split := strings.Split(path, "/")
	return split[len(split)-1]
}

func GenerateHandlerUUID() (uint32, error) {
	uuid := make([]byte, 16)
	_, err := rand.Read(uuid)
	if err != nil {
		return 0, err
	}
	return crc32.ChecksumIEEE(uuid), nil
}

type GocbLogger struct{}

func (r *GocbLogger) Log(level gocb.LogLevel, offset int, format string, v ...interface{}) error {
	format = "[gocb] " + format
	switch level {
	case gocb.LogError:
		logging.Errorf(format, v...)
	case gocb.LogWarn:
		logging.Warnf(format, v...)
	case gocb.LogInfo:
		logging.Infof(format, v...)
	case gocb.LogDebug:
		logging.Debugf(format, v...)
	case gocb.LogTrace:
		logging.Tracef(format, v...)
	case gocb.LogSched:
		logging.Tracef(format, v...)
	case gocb.LogMaxVerbosity:
		logging.Tracef(format, v...)
	default:
		logging.Tracef(format, v...)
	}
	return nil
}
