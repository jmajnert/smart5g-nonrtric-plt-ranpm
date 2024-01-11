//  ============LICENSE_START===============================================
//  Copyright (C) 2023 Nordix Foundation. All rights reserved.
//  ========================================================================
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//  ============LICENSE_END=================================================
//

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"encoding/csv"

	log "github.com/sirupsen/logrus"

	"github.com/gorilla/mux"

	"net/http/pprof"
	zmq "github.com/pebbe/zmq4"
)

//== Constants ==//

const https_port = 443

const shared_files = "/files"
const template_files = "/template-files"
const scenario_files = "/scenario-files"

const file_template = "pm-template.xml.gz"
const scenario_file_template = "scenario-pm-template.xml.gz"
const scenario_data_file = "demo_data.csv"

var always_return_file = os.Getenv("ALWAYS_RETURN")
var generated_files_start_time = os.Getenv("GENERATED_FILE_START_TIME")
var generated_files_timezone = os.Getenv("GENERATED_FILE_TIMEZONE")

var unzipped_template = ""

type record struct {
	eId string
	cId string
	datarate string
}

// testbed data are in format:
// "eid1,cid1,datarate1,eid2,cid2,datarate2,...,eidN,cidN,datarateN,"
func parseTestbedData(s string) []record {
	var records []record
	split := strings.Split(s, ",")
	if (len(split) - 1) % 3 != 0 {
		log.Error("Data incomplete");
		return records
	}
	for i := 0; i < len(split) -1; i = i+3 {
		r := record{split[i], split[i+1], split[i+2]}
		records = append(records, r)
	}
	return records
}

func getTestbedData() string {
	report_header := "datarate_report"
	zmq_address := "tcp://127.0.0.1:6671"

	subscriber, _ := zmq.NewSocket(zmq.SUB)
	defer subscriber.Close()
	subscriber.Connect(zmq_address)
	subscriber.SetSubscribe(report_header)

	log.Info("ZMQ subscriber created and connected")

	reply, err := subscriber.RecvMessage(zmq.DONTWAIT)
	if err != nil {
		// may consider better error handling
		log.Error("Receiving ZMQ message failed")
		return ""
	}
	if len(reply) > 0 {
		return string(reply[0])
	}
	return ""
}

// maps cell_id to [cell_max, cell_prev]
var data = make(map[string][]int)

// takes cellIds and returns PRB usage for them
// PRB Usage calculated as current_datarate/max_datarate for a given cell
func getPrbUsage(cell_ids []string) []string {
	testbed_data := getTestbedData()
	// update cache with current data
	if testbed_data != "" {
		records := parseTestbedData(testbed_data)
		for _, r := range records {
			current_datarate, err := strconv.Atoi(r.datarate)
			if err != nil {
				log.Error("Error converting datarate to number: ", r.datarate)
				continue
			}
			_, ok := data[r.cId]
			if ok == false {
				data[r.cId] = make([]int, 2)
				data[r.cId][0] = current_datarate
			} else {
				if data[r.cId][0] < current_datarate {
					data[r.cId][0] = current_datarate
				}
			}
			data[r.cId][1] = current_datarate
		}
	}
	// get data for requested cells
	prbs := make([]string, len(cell_ids))
	for i, cell_id := range cell_ids {
		prb := "0"
		cell_data, ok := data[cell_id]
		if ok == true {
			if cell_data[0] == 0 {
				prb = "0"
			} else {
				prb = strconv.Itoa(cell_data[1] * 100 / cell_data[0])
			}
		}
		prbs[i] = prb
	}
	return prbs
}

// Get static file
// Returned file is based on configuration
// If env ALWAYS_RETURN points to file under "/files", then that file is returned regardless of requested file
// Otherwise the requested file is returned if it exists
// If the requested file has a file name prefix of "NONEXISTING",then just 404 is returned
func files(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(req)

	if id, ok := vars["fileid"]; ok {
		if strings.HasPrefix(id, "NONEXISTING") {
			w.WriteHeader(http.StatusNotFound)
		}
		fn := shared_files + "/" + id
		if always_return_file != "" {
			fn = always_return_file
		}
		fileBytes, err := os.ReadFile(fn)
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(fileBytes)

		log.Info("File retrieval : ", fn, "as ", id, ", time:", time.Since(start).String())
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

// Unzip data from a reader and push to a writer
func gunzipReaderToWriter(w io.Writer, data io.Reader) error {
	gr, err1 := gzip.NewReader(data)

	if err1 != nil {
		return err1
	}
	defer gr.Close()
	data2, err2 := io.ReadAll(gr)
	if err2 != nil {
		return err2
	}
	_, err3 := w.Write(data2)
	if err3 != nil {
		return err3
	}
	return nil
}

// Zip the contents of a byte buffer and push to a writer
func gzipWrite(w io.Writer, data *[]byte) error {
	gw, err1 := gzip.NewWriterLevel(w, gzip.BestSpeed)

	if err1 != nil {
		return err1
	}
	defer gw.Close()
	_, err2 := gw.Write(*data)
	return err2
}

// Get generated file
// Returns a file generated from a template
// The requested file shall be according to this fileformat:
// A<year><month><day>.<hour><minute><timezone>-<hour><minute><timezone>_<nodename>.xml.gz
// Example: A20230220.1400+0100-1415+0100_GNODEBX-332.xml.gz
// The date and time shall be equal to or later then the date time configured start time in env GENERATED_FILE_START_TIME
// If the requested start time is earlier then the configured starttime, 404 is returned
// The returned file has nodename, start and end time set according to the requested file.
// In addition, the counter values are set to value representing the number of 15 min perioids since the
// configured start time
func generatedfiles(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(req)

	if id, ok := vars["fileid"]; ok {
		if strings.HasPrefix(id, "NONEXISTING") {
			w.WriteHeader(http.StatusNotFound)
		}
		log.Debug("Request generated file:", id)
		timezone := "+0000"
		if generated_files_timezone != "" {
			timezone = generated_files_timezone
			log.Debug("Configured timezone: ", timezone)
		} else {
			log.Debug("Using default timezone: ", timezone)
		}

		fn := template_files + "/" + file_template
		if unzipped_template == "" {

			var buf3 bytes.Buffer
			file, err := os.Open(fn)
			if err != nil {
				log.Error("PM template file", file_template, " does not exist")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			errb := gunzipReaderToWriter(&buf3, bufio.NewReader(file))
			if errb != nil {
				log.Error("Cannot gunzip file ", file_template, " - ", errb)
				return
			}

			unzipped_template = string(buf3.Bytes())

		}

		//Parse file start date/time
		//Example: 20230220.1300

		layout := "20060102.1504"
		tref, err := time.Parse(layout, generated_files_start_time)
		if err != nil {
			log.Error("Env var GENERATED_FILE_START_TIME cannot be parsed")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		//Parse file name start date/time

		ts := id[1:14]
		tcur, err := time.Parse(layout, ts)
		if err != nil {
			log.Error("File start date/time cannot be parsed: ", ts)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Calculate file index based of reference time
		file_index := (tcur.Unix() - tref.Unix()) / 900
		if file_index < 0 {
			log.Error("File start date/time before value of env var GENERATED_FILE_START_TIME :", generated_files_start_time)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		//begintime/endtime format: 2022-04-18T19:00:00+00:00
		begintime := tcur.Format("2006-01-02T15:04") + ":00" + timezone
		d := time.Duration(900 * time.Second)
		tend := tcur.Add(d)
		endtime := tend.Format("2006-01-02T15:04") + ":00" + timezone

		//Extract nodename
		nodename := id[30:]
		nodename = strings.Split(nodename, ".")[0]

		template_string := strings.Clone(unzipped_template)

		log.Debug("Replacing BEGINTIME with: ", begintime)
		log.Debug("Replacing ENDTIME with: ", endtime)
		log.Debug("Replacing CTR_VALUE with: ", strconv.Itoa(int(file_index)))
		log.Debug("Replacing NODE_NAME with: ", nodename)

		template_string = strings.Replace(template_string, "BEGINTIME", begintime, -1)
		template_string = strings.Replace(template_string, "ENDTIME", endtime, -1)

		template_string = strings.Replace(template_string, "CTR_VALUE", strconv.Itoa(int(file_index)), -1)

		template_string = strings.Replace(template_string, "NODE_NAME", nodename, -1)

		b := []byte(template_string)
		var buf bytes.Buffer
		err = gzipWrite(&buf, &b)
		if err != nil {
			log.Error("Cannot gzip file ", id, " - ", err)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buf.Bytes())

		log.Info("File retrieval generated file: ", fn, "as ", id, ", time:", time.Since(start).String())
		return
	}

	w.WriteHeader(http.StatusNotFound)
}


func scenario(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	vars := mux.Vars(req)

	if id, ok := vars["fileid"]; ok {
		if strings.HasPrefix(id, "NONEXISTING") {
			w.WriteHeader(http.StatusNotFound)
		}
		log.Debug("Request scenario file:", id)
		timezone := "+0000"
		if generated_files_timezone != "" {
			timezone = generated_files_timezone
			log.Debug("Configured timezone: ", timezone)
		} else {
			log.Debug("Using default timezone: ", timezone)
		}

		fn := scenario_files + "/" + scenario_file_template
		if unzipped_template == "" {

			var buf3 bytes.Buffer
			file, err := os.Open(fn)
			if err != nil {
				log.Error("PM template file", scenario_file_template, " does not exist")
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			errb := gunzipReaderToWriter(&buf3, bufio.NewReader(file))
			if errb != nil {
				log.Error("Cannot gunzip file ", scenario_file_template, " - ", errb)
				return
			}

			unzipped_template = string(buf3.Bytes())

		}

		//Parse file start date/time
		//Example: 20230220.130505

		layout := "20060102.150405"
		ts := id[1:16]
		tcur, err := time.Parse(layout, ts)
		if err != nil {
			log.Error("File start date/time cannot be parsed: ", ts)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		//begintime/endtime format: 2022-04-18T19:00:00+00:00
		begintime := tcur.Format("2006-01-02T15:04:05") + timezone
		d := time.Duration(1 * time.Second)
		tend := tcur.Add(d)
		endtime := tend.Format("2006-01-02T15:04:05") + timezone

		//get counter values from testbed's zmq
		prb_data := getPrbUsage([]string{"14550001", "1454c001"})
		log.Info("PRBUsage data:", prb_data)
		ctr_value_1 := prb_data[0]
		ctr_value_2 := prb_data[1]

		//get counter values from ransim  - pmRrcConnectedUes
		//dummy values for now
		ctr_value_3 := "0"
		ctr_value_4 := "8"

		//Extract nodename
		nodename := id[34:]
		nodename = strings.Split(nodename, ".")[0]

		template_string := strings.Clone(unzipped_template)

		log.Debug("Replacing BEGINTIME with: ", begintime)
		log.Debug("Replacing ENDTIME with: ", endtime)
		log.Debug("Replacing CTR_VALUE_1 with: ", ctr_value_1)
		log.Debug("Replacing CTR_VALUE_2 with: ", ctr_value_2)
		log.Debug("Replacing CTR_VALUE_3 with: ", ctr_value_3)
		log.Debug("Replacing CTR_VALUE_4 with: ", ctr_value_4)
		log.Debug("Replacing NODE_NAME with: ", nodename)

		template_string = strings.Replace(template_string, "BEGINTIME", begintime, -1)
		template_string = strings.Replace(template_string, "ENDTIME", endtime, -1)

		template_string = strings.Replace(template_string, "CTR_VALUE_1", ctr_value_1, -1)
		template_string = strings.Replace(template_string, "CTR_VALUE_2", ctr_value_2, -1)
		template_string = strings.Replace(template_string, "CTR_VALUE_3", ctr_value_3, -1)
		template_string = strings.Replace(template_string, "CTR_VALUE_4", ctr_value_4, -1)

		template_string = strings.Replace(template_string, "NODE_NAME", nodename, -1)

		b := []byte(template_string)
		var buf bytes.Buffer
		err = gzipWrite(&buf, &b)
		if err != nil {
			log.Error("Cannot gzip file ", id, " - ", err)
			return
		}

		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buf.Bytes())

		log.Info("File retrieval scenario file: ", fn, "as ", id)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

// Simple alive check
func alive(w http.ResponseWriter, req *http.Request) {
	//Alive check
}

// == Main ==//
func main() {

	//log.SetLevel(log.InfoLevel)
	log.SetLevel(log.TraceLevel)

	log.Info("Server starting...")

	rtr := mux.NewRouter()
	rtr.HandleFunc("/files/{fileid}", files)
	rtr.HandleFunc("/generatedfiles/{fileid}", generatedfiles)
	rtr.HandleFunc("/scenario/{fileid}", scenario)
	rtr.HandleFunc("/", alive)

	rtr.HandleFunc("/custom_debug_path/profile", pprof.Profile)

	http.Handle("/", rtr)

	// Run https
	log.Info("Starting https service...")
	err := http.ListenAndServeTLS(":"+strconv.Itoa(https_port), "certs/server.crt", "certs/server.key", nil)
	if err != nil {
		log.Fatal("Cannot setup listener on https: ", err)
	}

	//Wait until all go routines has exited
	runtime.Goexit()

	log.Warn("main routine exit")
	log.Warn("server i stopping...")
}
