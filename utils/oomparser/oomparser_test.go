// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package oomparser

import (
	"os"
	"testing"
	"time"
)

const startLine = "Jan 21 22:01:49 localhost kernel: [62278.816267] ruby invoked oom-killer: gfp_mask=0x201da, order=0, oom_score_adj=0"
const endLine = "Jan 21 22:01:49 localhost kernel: [62279.421192] Killed process 19667 (evilprogram2) total-vm:1460016kB, anon-rss:1414008kB, file-rss:4kB"
const containerLine = "Jan 26 14:10:07 kateknister0.mtv.corp.google.com kernel: [1814368.465205] Task in /mem2 killed as a result of limit of /mem2"
const containerLogFile = "containerOomExampleLog.txt"
const systemLogFile = "systemOomExampleLog.txt"

func createExpectedContainerOomInstance(t *testing.T) *OomInstance {
	deathTime, err := time.Parse(time.Stamp, "Jan  5 15:19:27")
	if err != nil {
		t.Fatalf("could not parse expected time when creating expected container oom instance. Had error %v", err)
		return nil
	}
	return &OomInstance{
		Pid:           13536,
		ProcessName:   "memorymonster",
		TimeOfDeath:   deathTime,
		ContainerName: "/mem2",
	}
}

func createExpectedSystemOomInstance(t *testing.T) *OomInstance {
	deathTime, err := time.Parse(time.Stamp, "Jan 28 19:58:45")
	if err != nil {
		t.Fatalf("could not parse expected time when creating expected system oom instance. Had error %v", err)
		return nil
	}
	return &OomInstance{
		Pid:           1532,
		ProcessName:   "badsysprogram",
		TimeOfDeath:   deathTime,
		ContainerName: "/",
	}
}

func TestGetContainerName(t *testing.T) {
	currentOomInstance := new(OomInstance)
	err := getContainerName(startLine, currentOomInstance)
	if err != nil {
		t.Errorf("bad line fed to getContainerName should yield no error, but had error %v", err)
	}
	if currentOomInstance.ContainerName != "" {
		t.Errorf("bad line fed to getContainerName yielded no container name but set it to %s", currentOomInstance.ContainerName)
	}
	err = getContainerName(containerLine, currentOomInstance)
	if err != nil {
		t.Errorf("container line fed to getContainerName should yield no error, but had error %v", err)
	}
	if currentOomInstance.ContainerName != "/mem2" {
		t.Errorf("getContainerName should have set containerName to /mem2, not %s", currentOomInstance.ContainerName)
	}
}

func TestGetProcessNamePid(t *testing.T) {
	currentOomInstance := new(OomInstance)
	couldParseLine, err := getProcessNamePid(startLine, currentOomInstance)
	if err != nil {
		t.Errorf("bad line fed to getProcessNamePid should yield no error, but had error %v", err)
	}
	if couldParseLine {
		t.Errorf("bad line fed to getProcessNamePid should return false but returned %v", couldParseLine)
	}

	correctTime, err := time.Parse(time.Stamp, "Jan 21 22:01:49")
	couldParseLine, err = getProcessNamePid(endLine, currentOomInstance)
	if err != nil {
		t.Errorf("good line fed to getProcessNamePid should yield no error, but had error %v", err)
	}
	if !couldParseLine {
		t.Errorf("good line fed to getProcessNamePid should return true but returned %v", couldParseLine)
	}
	if currentOomInstance.ProcessName != "evilprogram2" {
		t.Errorf("getProcessNamePid should have set processName to evilprogram2, not %s", currentOomInstance.ProcessName)
	}
	if currentOomInstance.Pid != 19667 {
		t.Errorf("getProcessNamePid should have set PID to 19667, not %d", currentOomInstance.Pid)
	}
	if !correctTime.Equal(currentOomInstance.TimeOfDeath) {
		t.Errorf("getProcessNamePid should have set date to %v, not %v", correctTime, currentOomInstance.Pid)
	}
}

func TestCheckIfStartOfMessages(t *testing.T) {
	couldParseLine, err := checkIfStartOfOomMessages(endLine)
	if err != nil {
		t.Errorf("bad line fed to checkIfStartOfMessages should yield no error, but had error %v", err)
	}
	if couldParseLine {
		t.Errorf("bad line fed to checkIfStartOfMessages should return false but returned %v", couldParseLine)
	}

	couldParseLine, err = checkIfStartOfOomMessages(startLine)
	if err != nil {
		t.Errorf("start line fed to checkIfStartOfMessages should yield no error, but had error %v", err)
	}
	if !couldParseLine {
		t.Errorf("start line fed to checkIfStartOfMessages should return true but returned %v", couldParseLine)
	}
}

func TestAnalyzeLinesContainerOom(t *testing.T) {
	expectedContainerOomInstance := createExpectedContainerOomInstance(t)
	helpTestAnalyzeLines(expectedContainerOomInstance, containerLogFile, t)
}

func TestAnalyzeLinesSystemOom(t *testing.T) {
	expectedSystemOomInstance := createExpectedSystemOomInstance(t)
	helpTestAnalyzeLines(expectedSystemOomInstance, systemLogFile, t)
}

func helpTestAnalyzeLines(oomCheckInstance *OomInstance, sysFile string, t *testing.T) {
	outStream := make(chan *OomInstance)
	oomLog := new(OomParser)
	oomLog.systemFile = sysFile
	file, err := os.Open(oomLog.systemFile)
	if err != nil {
		t.Errorf("couldn't open test log: %v", err)
	}
	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(1 * time.Second)
		timeout <- true
	}()
	go oomLog.analyzeLines(file, outStream)
	select {
	case oomInstance := <-outStream:
		if *oomCheckInstance != *oomInstance {
			t.Errorf("wrong instance returned. Expected %v and got %v",
				oomCheckInstance, oomInstance)
		}
	case <-timeout:
		t.Error(
			"timeout happened before oomInstance was found in test file")
	}
}

func TestStreamOomsContainer(t *testing.T) {
	expectedContainerOomInstance := createExpectedContainerOomInstance(t)
	helpTestStreamOoms(expectedContainerOomInstance, containerLogFile, t)
}

func TestStreamOomsSystem(t *testing.T) {
	expectedSystemOomInstance := createExpectedSystemOomInstance(t)
	helpTestStreamOoms(expectedSystemOomInstance, systemLogFile, t)
}

func helpTestStreamOoms(oomCheckInstance *OomInstance, sysFile string, t *testing.T) {
	outStream := make(chan *OomInstance)
	oomLog := new(OomParser)
	oomLog.systemFile = sysFile
	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(1 * time.Second)
		timeout <- true
	}()

	err := oomLog.StreamOoms(outStream)
	if err != nil {
		t.Errorf("had an error opening file: %v", err)
	}

	select {
	case oomInstance := <-outStream:
		if *oomCheckInstance != *oomInstance {
			t.Errorf("wrong instance returned. Expected %v and got %v",
				oomCheckInstance, oomInstance)
		}
	case <-timeout:
		t.Error(
			"timeout happened before oomInstance was found in test file")
	}
}

func TestNew(t *testing.T) {
	_, err := New()
	if err != nil {
		t.Errorf("function New() had error %v", err)
	}
}
