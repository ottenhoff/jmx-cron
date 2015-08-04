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
	"strings"
	"time"

	"github.com/alexcesaro/log/stdlog"
)

const adminURL = "https://admin.longsight.com/longsight/json/jmx-instances"
const cronUserAgent = "JMX-Cron v1.0"

var token = flag.String("token", "", "the custom security token")
var localIP = flag.String("ips", "", "ips to check")
var clientID = flag.String("clientID", "", "client id")

//var propertyFiles = [4]string{"instance.properties", "dev.properties", "local.properties", "sakai.properties"}
var logger = stdlog.GetFromFlags()
var outputBuffer bytes.Buffer

// TomcatInstance is a tomcat instance from the Longsight admin portal
type TomcatInstance struct {
	ServerID    string
	JvmRoute    string
	ServerIP    string
	JmxPort     string
	ProjectID   string
	ProjectName string
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
}

func main() {
	logger.Debug("Auto-detected IPs on this server")
	instances := getInstancesFromPortal()

	// This is the channel the responses will come back on
	responseChannel := make(chan TomcatCheckResult)

	for _, TomcatInstance := range instances {
		go getResponseTime(responseChannel, TomcatInstance)
		logger.Debug("URL to test: ")
	}
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
	urlToTest := "http://" + tomcat.ServerIP + ":" + tomcat.JmxPort + "/"
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
	requestTime := time.Since(timeStart).String()
	httpOK := false

	if err != nil {
		log.Printf("Error fetching: %v", err)
		httpOK = false
	} else {
		defer resp.Body.Close()

		logger.Debug("Request time and status:", requestTime, resp.StatusCode)
		if resp.StatusCode == http.StatusOK {
			httpOK = true
		}
	}

	// Send our results back to the main processes via our return channel
	returnChannel <- TomcatCheckResult{tomcat.ServerID, httpOK, requestTime}
}
