package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/alexcesaro/log/stdlog"
)

const adminURL = "https://admin.longsight.com/longsight/json/jmx-instances"
const cronUserAgent = "JMX-Cron v1.0"

var token = flag.String("token", "", "the custom security token")
var localIP = flag.String("ips", "", "ips to check")
var clientID = flag.String("clientID", "", "client id")
var jolokiaURL = flag.String("jolokia", "http://10.4.100.101:32222/jolokia", "Jolokia endpoint")

//var propertyFiles = [4]string{"instance.properties", "dev.properties", "local.properties", "sakai.properties"}
var logger = stdlog.GetFromFlags()
var outputBuffer bytes.Buffer

// TomcatInstance is a tomcat instance from the Longsight admin portal
type TomcatInstance struct {
	ServerID    string
	JvmRoute    string
	ServerIP    string
	HTTPPort    string
	JmxPort     string
	ProjectID   string
	ProjectName string
}

// JolokiaReadResponse is the JSON-encoded info return from the Jolokia JMX proxy
type JolokiaReadResponse struct {
	Status    uint32
	Timestamp uint32
	Request   map[string]interface{}
	Value     map[string]interface{}
	Error     string
}

// TomcatCheckResult is returned from async call
type TomcatCheckResult struct {
	ServerID       string
	ServerStatus   bool
	DataType       string
	ServerResponse string
}

// JolokiaRequest gets POSTed to Jolokia
type JolokiaRequest struct {
	Type      string `json:"type"`
	Mbean     string `json:"mbean"`
	Attribute string `json:"attribute"`
	Path      string `json:"path"`
	Target    struct {
		URL string `json:"url"`
	} `json:"target"`
}

// JolokiaRequestResponse Auto-gen from http://mholt.github.io/json-to-go/
type JolokiaRequestResponse []struct {
	Timestamp int `json:"timestamp"`
	Status    int `json:"status"`
	Request   struct {
		Mbean  string `json:"mbean"`
		Path   string `json:"path"`
		Target struct {
			URL string `json:"url"`
		} `json:"target"`
		Attribute string `json:"attribute"`
		Type      string `json:"type"`
	} `json:"request"`
	Value int64 `json:"value"`
}

func init() {
	flag.Parse()
	if len(*token) < 1 {
		fmt.Println("Please provide a valid security token")
		os.Exit(1)
	}

	// Limit the request concurrency
	runtime.GOMAXPROCS(runtime.NumCPU())
}

func main() {
	logger.Debug("Auto-detected IPs on this server")
	instances := getInstancesFromPortal()

	// This is the channel the responses will come back on
	responseChannel := make(chan TomcatCheckResult)

	for _, TomcatInstance := range instances {
		urlToTest := "http://" + TomcatInstance.ServerIP + ":" + TomcatInstance.HTTPPort + "/"
		if strings.Contains(TomcatInstance.ProjectName, "sakai") {
			urlToTest += "portal/xlogin"
		}

		go getResponseTime(responseChannel, TomcatInstance, urlToTest)
		logger.Debug("URL to test: ", TomcatInstance)
	}

	// Wait for all the goroutines to finish, collecting the responses
	tomcatCheckMapping := waitForDomains(responseChannel, len(instances))

	for _, TomcatInstance := range instances {
		// TODO: make this concurrent
		getJmxAttributes(TomcatInstance.ServerIP + ":" + TomcatInstance.JmxPort)
	}

	// Send the info back to admin portal
	updateAdminPortal(tomcatCheckMapping)
	logger.Debug("Final result:", tomcatCheckMapping)
}

func getInstancesFromPortal() []TomcatInstance {
	var tomcatInstances []TomcatInstance
	url := adminURL + "?1=1"
	if len(*localIP) > 1 {
		url += "&ips=" + *localIP
	}
	if len(*clientID) > 1 {
		url += "&clientID=" + *clientID
	}

	req, err := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Auth-Token", *token)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", cronUserAgent)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)

		// We have real info
		if len(body) > 5 {
			json.Unmarshal(body, &tomcatInstances)
			logger.Debug("Raw data from admin portal: ", tomcatInstances)
		}
	} else {
		logger.Alertf("Bad HTTP fetch: %v \n", resp.Status)
		os.Exit(1)
	}

	return tomcatInstances
}

func getResponseTime(returnChannel chan TomcatCheckResult, tomcat TomcatInstance, urlToTest string) {
	client := http.Client{
		Timeout: time.Duration(1 * time.Second),
	}
	client.Get(urlToTest)

	timeStart := time.Now()
	resp, err := http.Get(urlToTest)
	requestTime := strconv.FormatInt(time.Since(timeStart).Nanoseconds()/1000, 10)
	httpOK := false

	if err != nil {
		log.Printf("Error fetching: %v", err)
		httpOK = false
	} else {
		defer resp.Body.Close()

		logger.Debug("Request time:", urlToTest, requestTime, resp.StatusCode)
		if resp.StatusCode == http.StatusOK {
			httpOK = true
		}
	}

	// Send our results back to the main processes via our return channel
	returnChannel <- TomcatCheckResult{tomcat.ServerID, httpOK, "time", requestTime}
}

// The extra set of parentheses here are the return type. You can give the return value a name,
// in this case +tomcatCheckMapping+ and use that name in the function body. Then you don't need to specify
// what actually gets returned, you've already defined it here.
func waitForDomains(responseChannel chan TomcatCheckResult, instanceCount int) (tomcatCheckMapping []TomcatCheckResult) {
	returnedCount := 0
	for {
		tomcatCheckMapping = append(tomcatCheckMapping, <-responseChannel)
		returnedCount++

		if returnedCount >= instanceCount {
			break
		}
	}

	return
}

func getJmxAttributes(jmxURL string) (*JolokiaRequestResponse, error) {
	//jsonRequest := "{\"attribute\":\"DaemonThreadCount,HeapMemoryUsage,ThreadCount,MaxFileDescriptorCount,OpenFileDescriptorCount,ProcessCpuTime\","
	//jsonRequest += "\"mbean\":\"java.lang:type=*\",\"target\":{\"url\":\"service:jmx:rmi:///jndi/rmi://10.4.100.215:51889/jmxrmi\"},\"type\":\"READ\"}"

	heapRequest := JolokiaRequest{
		Type:      "READ",
		Mbean:     "java.lang:type=Memory",
		Attribute: "HeapMemoryUsage",
		Path:      "used",
		Target: struct {
			URL string `json:"url"`
		}{URL: "service:jmx:rmi:///jndi/rmi://10.4.100.215:51889/jmxrmi"},
	}

	threadRequest := JolokiaRequest{
		Type:      "READ",
		Mbean:     "java.lang:type=Threading",
		Attribute: "ThreadCount",
		Target: struct {
			URL string `json:"url"`
		}{URL: "service:jmx:rmi:///jndi/rmi://10.4.100.215:51889/jmxrmi"},
	}

	cpuRequest := JolokiaRequest{
		Type:      "READ",
		Mbean:     "java.lang:type=OperatingSystem",
		Attribute: "ProcessCpuTime",
		Target: struct {
			URL string `json:"url"`
		}{URL: "service:jmx:rmi:///jndi/rmi://10.4.100.215:51889/jmxrmi"},
	}

	sakaiSessionRequest := JolokiaRequest{
		Type:      "READ",
		Mbean:     "org.sakaiproject:name=Sessions",
		Attribute: "Active15Min",
		Target: struct {
			URL string `json:"url"`
		}{URL: "service:jmx:rmi:///jndi/rmi://" + jmxURL + "/jmxrmi"},
	}

	var requestArray [4]JolokiaRequest
	requestArray[0] = heapRequest
	requestArray[1] = threadRequest
	requestArray[2] = cpuRequest
	requestArray[3] = sakaiSessionRequest

	jsonRequest, err := json.Marshal(requestArray)
	if err != nil {
		panic("Could not marshal json for jolokia request")
	}
	logger.Debug("json: " + string(jsonRequest))

	client := &http.Client{
		Timeout: time.Duration(2 * time.Second),
	}
	req, _ := http.NewRequest("POST", *jolokiaURL, strings.NewReader(string(jsonRequest)))
	req.Header.Set("User-Agent", cronUserAgent)
	resp, respErr := client.Do(req)

	if respErr != nil {
		return nil, respErr
	}
	defer resp.Body.Close()

	var respJ JolokiaRequestResponse
	dec := json.NewDecoder(resp.Body)

	//contents, _ := ioutil.ReadAll(resp.Body)
	//logger.Debug("Raw body:", string(contents), dec)

	if err := dec.Decode(&respJ); err != nil {
		return nil, err
	}

	jResponse := &respJ
	for _, jResp := range *jResponse {
		mbean := jResp.Request.Mbean
		v := jResp.Value
		logger.Debug("response value: ", mbean, v)
	}

	return &respJ, nil
}

func updateAdminPortal(tomcatChecks []TomcatCheckResult) {
	jsonData, err := json.Marshal(tomcatChecks)
	if err != nil {
		panic(err)
	}

	// Unix time converted to a string
	//currentTime := strconv.FormatInt(time.Now().Unix(), 10)

	postURL := "https://admin.longsight.com/longsight/go/healthinfo"
	//urlValues := url.Values{"time": {string(currentTime)}, "data": {string(jsonData)}}
	logger.Debug("Values being sent to admin portal: ", string(jsonData))

	client := &http.Client{}
	req, _ := http.NewRequest("POST", postURL, strings.NewReader(string(jsonData)))
	req.Header.Set("X-Auth-Token", *token)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("User-Agent", cronUserAgent)
	resp, err := client.Do(req)

	logger.Debug("Response from admin portal: ", resp)

	if err != nil {
		panic("Could not POST update")
	}
}
