package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
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
	Value     interface{}
	Error     string
}

// TomcatCheckResult is returned from async call
type TomcatCheckResult struct {
	ServerID       string
	ServerStatus   bool
	ServerResponse string
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
		go getResponseTime(responseChannel, TomcatInstance)
		logger.Debug("URL to test: ", TomcatInstance)
	}

	// Wait for all the goroutines to finish, collecting the responses
	tomcatCheckMapping := waitForDomains(responseChannel, len(instances))
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

func getResponseTime(returnChannel chan TomcatCheckResult, tomcat TomcatInstance) {
	urlToTest := "http://" + tomcat.ServerIP + ":" + tomcat.HTTPPort + "/"
	if strings.Contains(tomcat.ProjectName, "sakai") {
		urlToTest += "portal/"
	}

	timeout := time.Duration(7 * time.Second)
	client := http.Client{
		Timeout: timeout,
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
	returnChannel <- TomcatCheckResult{tomcat.ServerID, httpOK, requestTime}
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

/*
func GetAttr(service, domain, bean, attr string) (interface{}, error) {
	resp, err := getAttr(service + "/jolokia/read/" + domain + ":" + bean + "/" + attr)
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}


func getAttr(jURL string) (*JolokiaReadResponse, error) {
	jsonRequest := "{\"attribute\":\"DaemonThreadCount,HeapMemoryUsage,ThreadCount,MaxFileDescriptorCount,OpenFileDescriptorCount,ProcessCpuTime\","
	jsonRequest += "\"mbean\":\"java.lang:type=*\",\"target\":{\"url\":\"service:jmx:rmi:///jndi/rmi://10.4.100.215:51889/jmxrmi\"},\"type\":\"READ\"}"

	resp, respErr := http.Post(jURL, url.Values{jsonRequest})
	if respErr != nil {
		return nil, respErr
	}
	defer resp.Body.Close()
	var respJ JolokiaReadResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&respJ); err != nil {
		return nil, err
	}
	return &respJ, nil
}
*/

func updateAdminPortal(tomcatChecks []TomcatCheckResult) {
	jsonData, err := json.Marshal(tomcatChecks)
	if err != nil {
		panic(err)
	}

	// Unix time converted to a string
	currentTime := strconv.FormatInt(time.Now().Unix(), 10)

	postURL := "https://admin.longsight.com/longsight/healthinfo"
	urlValues := url.Values{"time": {string(currentTime)}, "data": {string(jsonData)}}
	logger.Debug("Values being sent to admin portal: ", urlValues)

	resp, err := http.PostForm(postURL, urlValues)
	logger.Debug("Response from admin portal: ", resp)

	if err != nil {
		panic("Could not POST update")
	}
}
