package main

// This is a script that creates a web server to receive push calls from the
// Catchpoint Alerts API and sends it to nagios.
//
// Script parameters:
//   --verbose														 : sets the output to verbose
//   --config=/path/to/my/json/config/file : Path to the configuration file
//   --dump-requests-dir="/var/log/pushapi": Path to a directory where you dump
//                                           each request's body
//
// Usage example:
//  - server (this application) side:
//    ./alert_receiver --verbose
//  - On the client side (to test):
//    CURLFORMAT='\ntime_namelookup:%{time_namelookup},\ntime_connect:%{time_connect},\ntime_appconnect:%{time_appconnect},\ntime_pretransfer:%{time_pretransfer},\ntime_redirect:%{time_redirect},\ntime_starttransfer:%{time_starttransfer},\ntime_total:%{time_total},\nnum_connects:%{num_connects},\nnum_redirects:%{num_redirects}\n'
//    curl  -X POST -d @/tmp/alert_api.xml http://127.0.0.1:8080/catchpoint/alerts --header "Content-Type:application/xml" -w "%${CURLFORMAT}"
//
// Recommendations:
//   Put this server behind a haproxy + an iptable that filter out all the
//   source IPs and rejects everything that is not on the correct endpoint
//   (example: /catchpoint/alerts) use the lb as a proxy and make the script
//   listens on 127.0.0.1 only.
//

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/tubemogul/catchpoint_api_sdk_go/alertAPI"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Those are the arguments you can pass to the script
var (
	// Flag to run or not the deamon in Verbose mode. Defaults to false.
	verbose = flag.Bool("verbose", false, "Set a verbose output")
	// Path to the configuration file. Defaults to "./receiver.cfg.json"
	configFile = flag.String("config", "./receiver.cfg.json", "Path to the config file")
	// Dumps the http requests content to 1 file each inside the provided
	// directory. If this string is empty it doesn't dump anything.
	// This is set as an application argument as it is generally used for debug
	// purpose only.
	dumpRequestsDir = flag.String("dump-requests-dir", "", "Dump each http request's body into a new file in the provided folder")
)

var config = new(Configuration)
var cache []string

// checkIpFiltering sends an empty response if an IP filtering is defined and
// the IP is out of this filter.
func checkIpFiltering(clientIP *string) bool {
	if len(config.AuthIPs) > 0 {
		client_ip := strings.Split(*clientIP, ":")[0]
		for _, autorized_ip := range strings.Split(config.AuthIPs, ",") {
			if client_ip == autorized_ip {
				logInfo(fmt.Sprintf("Accepted IP: %s", client_ip))
				return true
			}
		}

		logInfo(fmt.Sprintf("Refused IP: %s", *clientIP))
		return false
	}
	return true
}

// verifyRequestContent checks if the content of the request is empty. If yes,
// returns an HTTP error 400.
func verifyRequestContent(w *http.ResponseWriter, req *http.Request) bool {

	logInfo(fmt.Sprintf("Length of the query: %d", req.ContentLength))

	if req.ContentLength == 0 && req.Method != "GET" {
		http.Error(*w, http.StatusText(400), 400)
		return false
	}
	return true
}

// The handler that will redirect to the correct plugin
func genericHandler(w http.ResponseWriter, r *http.Request) {

	logInfo(fmt.Sprintf("Receiving a new query from %s on %s", r.RemoteAddr, r.URL.Path))

	// Doing nothing if the request is not from an authorized IP
	if !checkIpFiltering(&(r.RemoteAddr)) {
		return
	}

	// Doing nothing if the POST request is empty
	if !verifyRequestContent(&w, r) {
		return
	}

	body, readErr := ioutil.ReadAll(r.Body)
	handleErrorHttp(&readErr, &w)
	if readErr != nil {
		return
	}

	if len(*dumpRequestsDir) >= 0 {
		fName := fmt.Sprintf("%d_%d.txt", time.Now().UnixNano(), os.Getpid())
		err := ioutil.WriteFile(filepath.Join(*dumpRequestsDir, fName), body, 0644)
		logError(&err)
	}

	var (
		rc  uint8
		svc *string
		msg *[]string
		err error
	)

	var mutex = &sync.Mutex{}
	for _, endpoint := range config.Endpoints {
		if endpoint.URIPath == r.URL.Path {
			// Once you have the right endpoint, you check for the right plugin
			switch endpoint.PluginName {
			default:
				errCust := fmt.Errorf("Unsupported plugin name for %s", endpoint.PluginName)
				logError(&errCust)
				return
			case "catchpoint_alerts":
				plugin := new(alertsAPI.Alert)
				rc, svc, msg, err = plugin.RequestHandler(&body)

				// If there's an error un the handle of the request, logging the error
				// and exiting.
				handleErrorHttp(&err, &w)
				if err != nil {
					return
				}

				logInfo(fmt.Sprintf("Detected criticity = %d", rc))
				logInfo(fmt.Sprintf("Service = %s", *svc))
				logInfo(fmt.Sprintf("Msg = %+v", *msg))
			}

			// Sending NSCA messages if enabled
			if config.NSCA.Enabled {
				// We send an nsca alert for each failures in the test to have a better
				// report of the frequency of the failures
				for _, failure := range *msg {
					err := sendNscaMessage(&rc, svc, &failure)
					handleErrorHttp(&err, &w)
				}
			}
			// Sending check results to channel
			// And put in into the cache
			channel := make(chan string)
			go func() {
				for _, failure := range *msg {
					raw := Sensu{
						Status: rc,
						Name:   *svc,
						Output: failure,
					}
					res, err := json.Marshal(raw)
					if err != nil {
						handleErrorHttp(&err, &w)
					}
					channel <- string(res)
				}
				close(channel)
			}()
			// Preserve cache with Mutex
			mutex.Lock()
			for msg := range channel {
				cache = append(cache, msg)
			}
			mutex.Unlock()
			logInfo(fmt.Sprintf("%d items been written to the cache", len(cache)))
			break // break when you find the matching endpoint
		}
	}
	for _, sender := range config.Sender {
		if sender.URIPath == r.URL.Path {
			mutex.Lock()
			for _, v := range cache {
				fmt.Fprintf(w, "%q", v)
			}
			logInfo(fmt.Sprintf("%d items been read from the cache", len(cache)))
			// Discard cache after read
			cache = nil
			mutex.Unlock()
		}
	}
}

// Main function
func main() {
	flag.Parse()

	// load plugins

	// Loading the configuration
	logInfo("Loading config")
	err := config.loadConfig(*configFile)
	if err != nil {
		log.Fatal("Unable to laod configuration: %s", err)
	}

	// Multithreading the http server
	runtime.GOMAXPROCS(config.Procs)

	if len(config.LogFile) > 0 {
		logInfo(fmt.Sprintf("Setting the log output to %s", config.LogFile))
		f, err := os.OpenFile(config.LogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		logError(&err)
		defer f.Close()
		log.SetOutput(f)
	}

	// Default route. We use it to handle every request. The filtering out is done
	// in the handler
	// Creating channel
	http.HandleFunc("/", genericHandler)

	// Debug
	//	val := <-c // read from channel
	//	test, _ := json.Marshal(val)
	//	logInfo(fmt.Sprintf(string(test)))

	logInfo(fmt.Sprintf("Starting web server listening on %s:%d", config.IP, config.Port))
	s := &http.Server{
		Addr:           fmt.Sprintf("%s:%d", config.IP, config.Port),
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}
	log.Fatal(s.ListenAndServe())
}
