// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package docmanager helps persist documents state to disk
package docmanager

import (
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/docmanager/model"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
)

const (
	maxLogFileDeletions int = 100
)

type validString func(string) bool
type modifyString func(string) string

//TODO:  Revisit this when making Persistence invasive - i.e failure in file-systems should resort to Agent crash instead of swallowing errors

var lock sync.RWMutex
var docLock = make(map[string]*sync.RWMutex)

// GetDocumentInterimState returns CommandState object after reading file <fileName> from locationFolder
// under defaultLogDir/instanceID
func GetDocumentInterimState(log log.T, fileName, instanceID, locationFolder string) model.DocumentState {

	rLockDocument(fileName)
	defer rUnlockDocument(fileName)

	absoluteFileName := docStateFileName(fileName, instanceID, locationFolder)

	docState := getDocState(log, absoluteFileName)

	return docState
}

// PersistData stores the given object in the file-system in pretty Json indented format
// This will override the contents of an already existing file
func PersistData(log log.T, fileName, instanceID, locationFolder string, object interface{}) {

	lockDocument(fileName)
	defer unlockDocument(fileName)

	absoluteFileName := docStateFileName(fileName, instanceID, locationFolder)

	content, err := jsonutil.Marshal(object)
	if err != nil {
		log.Errorf("encountered error with message %v while marshalling %v to string", err, object)
	} else {
		if fileutil.Exists(absoluteFileName) {
			log.Debugf("overwriting contents of %v", absoluteFileName)
		}
		log.Tracef("persisting interim state %v in file %v", jsonutil.Indent(content), absoluteFileName)
		if s, err := fileutil.WriteIntoFileWithPermissions(absoluteFileName, jsonutil.Indent(content), os.FileMode(int(appconfig.ReadWriteAccess))); s && err == nil {
			log.Debugf("successfully persisted interim state in %v", locationFolder)
		} else {
			log.Debugf("persisting interim state in %v failed with error %v", locationFolder, err)
		}
	}
}

// IsDocumentCurrentlyExecuting checks if document already present in Pending or Current folder
func IsDocumentCurrentlyExecuting(fileName, instanceID string) bool {

	if len(fileName) == 0 {
		return false
	}

	lockDocument(fileName)
	defer unlockDocument(fileName)

	absoluteFileName := docStateFileName(fileName, instanceID, appconfig.DefaultLocationOfPending)
	if fileutil.Exists(absoluteFileName) {
		return true
	}
	absoluteFileName = docStateFileName(fileName, instanceID, appconfig.DefaultLocationOfCurrent)
	return fileutil.Exists(absoluteFileName)
}

// RemoveData deletes the fileName from locationFolder under defaultLogDir/instanceID
func RemoveData(log log.T, commandID, instanceID, locationFolder string) {

	absoluteFileName := docStateFileName(commandID, instanceID, locationFolder)

	err := fileutil.DeleteFile(absoluteFileName)
	if err != nil {
		log.Errorf("encountered error %v while deleting file %v", err, absoluteFileName)
	} else {
		log.Debugf("successfully deleted file %v", absoluteFileName)
	}
}

// MoveDocumentState moves the document file to target location
func MoveDocumentState(log log.T, fileName, instanceID, srcLocationFolder, dstLocationFolder string) {

	//get a lock for documentID specific lock
	lockDocument(fileName)

	absoluteSource := path.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultDocumentRootDirName,
		appconfig.DefaultLocationOfState,
		srcLocationFolder)

	absoluteDestination := path.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultDocumentRootDirName,
		appconfig.DefaultLocationOfState,
		dstLocationFolder)

	if s, err := fileutil.MoveFile(fileName, absoluteSource, absoluteDestination); s && err == nil {
		log.Debugf("moved file %v from %v to %v successfully", fileName, srcLocationFolder, dstLocationFolder)
	} else {
		log.Debugf("moving file %v from %v to %v failed with error %v", fileName, srcLocationFolder, dstLocationFolder, err)
	}

	//release documentID specific lock - before deleting the entry from the map
	unlockDocument(fileName)

	//delete documentID specific lock if document has finished executing. This is to avoid documentLock growing too much in memory.
	//This is done by ensuring that as soon as document finishes executing it is removed from documentLock
	//Its safe to assume that document has finished executing if it is being moved to appconfig.DefaultLocationOfCompleted
	if dstLocationFolder == appconfig.DefaultLocationOfCompleted {
		deleteLock(fileName)
	}
}

// GetDocumentInfo returns the document info for the specified fileName
func GetDocumentInfo(log log.T, fileName, instanceID, locationFolder string) model.DocumentInfo {
	rLockDocument(fileName)
	defer rUnlockDocument(fileName)

	absoluteFileName := docStateFileName(fileName, instanceID, locationFolder)

	commandState := getDocState(log, absoluteFileName)

	return commandState.DocumentInformation
}

// PersistDocumentInfo stores the given PluginState in file-system in pretty Json indented format
// This will override the contents of an already existing file
func PersistDocumentInfo(log log.T, docInfo model.DocumentInfo, fileName, instanceID, locationFolder string) {

	absoluteFileName := docStateFileName(fileName, instanceID, locationFolder)

	//get documentID specific write lock
	lockDocument(fileName)
	defer unlockDocument(fileName)

	//Plugins should safely assume that there already
	//exists a persisted interim state file - if not then it should throw error

	//read command state from file-system first
	commandState := getDocState(log, absoluteFileName)

	commandState.DocumentInformation = docInfo

	setDocState(log, commandState, absoluteFileName, locationFolder)
}

// GetPluginState returns PluginState after reading fileName from given locationFolder under defaultLogDir/instanceID
func GetPluginState(log log.T, pluginID, commandID, instanceID, locationFolder string) *model.PluginState {

	rLockDocument(commandID)
	defer rUnlockDocument(commandID)

	absoluteFileName := docStateFileName(commandID, instanceID, locationFolder)

	commandState := getDocState(log, absoluteFileName)

	for _, pluginState := range commandState.InstancePluginsInformation {
		if pluginState.Id == pluginID {
			return &pluginState
		}
	}

	return nil
}

// PersistPluginState stores the given PluginState in file-system in pretty Json indented format
// This will override the contents of an already existing file
func PersistPluginState(log log.T, pluginState model.PluginState, pluginID, commandID, instanceID, locationFolder string) {

	lockDocument(commandID)
	defer unlockDocument(commandID)

	absoluteFileName := docStateFileName(commandID, instanceID, locationFolder)

	//Plugins should safely assume that there already
	//exists a persisted interim state file - if not then it should throw error
	commandState := getDocState(log, absoluteFileName)

	//TODO:  after adding unit-tests for persist data - this can be removed
	if commandState.InstancePluginsInformation == nil {
		pluginsInfo := []model.PluginState{}
		pluginsInfo = append(pluginsInfo, pluginState)
		commandState.InstancePluginsInformation = pluginsInfo
	} else {
		for index, plugin := range commandState.InstancePluginsInformation {
			if plugin.Id == pluginID {
				commandState.InstancePluginsInformation[index] = pluginState
				break
			}
		}
	}

	setDocState(log, commandState, absoluteFileName, locationFolder)
}

// DocumentStateDir returns absolute filename where command states are persisted
func DocumentStateDir(instanceID, locationFolder string) string {
	return filepath.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultDocumentRootDirName,
		appconfig.DefaultLocationOfState,
		locationFolder)
}

// orchestrationDir returns the absolute path of the orchestration directory
func orchestrationDir(instanceID, orchestrationRootDirName string) string {
	return path.Join(appconfig.DefaultDataStorePath,
		instanceID,
		appconfig.DefaultDocumentRootDirName,
		orchestrationRootDirName)
}

// DeleteOldDocumentFolderLogs deletes the logs from document/state/completed and document/orchestration folders older than retention duration which satisfy the file name format
func DeleteOldDocumentFolderLogs(log log.T, instanceID, orchestrationRootDirName string, retentionDurationHours int, isIntendedFileNameFormat validString, formOrchestrationFolderName modifyString) {
	defer func() {
		// recover in case the function panics
		if msg := recover(); msg != nil {
			log.Errorf("DeleteOldDocumentFolderLogs failed with message %v", msg)
		}
	}()

	// Form the path for completed document state dir
	completedDir := DocumentStateDir(instanceID, appconfig.DefaultLocationOfCompleted)

	// Form the path for orchestration logs dir
	orchestrationRootDir := orchestrationDir(instanceID, orchestrationRootDirName)

	if !fileutil.Exists(completedDir) {
		log.Debugf("Completed log directory doesn't exist: %v", completedDir)
		return
	}

	completedFiles, err := fileutil.GetFileNames(completedDir)
	if err != nil {
		log.Debugf("Failed to read files under %v", err)
		return
	}

	if completedFiles == nil || len(completedFiles) == 0 {
		log.Debugf("Completed log directory %v is invalid or empty", completedDir)
		return
	}

	// Go through all log files in the completed logs dir, delete max maxLogFileDeletions files and the corresponding dirs from orchestration folder
	countOfDeletions := 0
	for _, completedFile := range completedFiles {

		completedLogFullPath := filepath.Join(completedDir, completedFile)

		//Checking for the file name format so that the function only deletes the files it is called to do. Also checking whether the file is beyond retention time.
		if isIntendedFileNameFormat(completedFile) && isOlderThan(log, completedLogFullPath, retentionDurationHours) {
			//The file name is valid for deletion and is also old. Go ahead for deletion.
			orchestrationFolder := formOrchestrationFolderName(completedFile)
			orchestrationDirFullPath := filepath.Join(orchestrationRootDir, orchestrationFolder)

			log.Debugf("Attempting Deletion of folder : %v", orchestrationDirFullPath)

			err := fileutil.DeleteDirectory(orchestrationDirFullPath)
			if err != nil {
				log.Debugf("Error deleting dir %v: %v", orchestrationDirFullPath, err)
				continue
			}

			// Deletion of orchestration dir was successful. Delete the document state file
			log.Debugf("Attempting Deletion of file : %v", completedLogFullPath)

			err = fileutil.DeleteDirectory(completedLogFullPath)

			if err != nil {
				log.Debugf("Error deleting file %v: %v", completedLogFullPath, err)
				continue
			}

			// Deletion of both document state and orchestration file was successful
			countOfDeletions += 2
			if countOfDeletions > maxLogFileDeletions {
				break
			}

		}

	}

	log.Debugf("Completed DeleteOldDocumentFolderLogs")
}

// isOlderThan checks whether the file is older than the retention duration
func isOlderThan(log log.T, fileFullPath string, retentionDurationHours int) bool {
	modificationTime, err := fileutil.GetFileModificationTime(fileFullPath)

	if err != nil {
		log.Debugf("Failed to get modification time %v", err)
		return false
	}

	// Check whether the current time is after modification time plus the retention duration
	return modificationTime.Add(time.Hour * time.Duration(retentionDurationHours)).Before(time.Now())
}

// getDocState reads commandState from given file
func getDocState(log log.T, fileName string) model.DocumentState {

	var commandState model.DocumentState
	err := jsonutil.UnmarshalFile(fileName, &commandState)
	if err != nil {
		log.Errorf("encountered error with message %v while reading Interim state of command from file - %v", err, fileName)
	} else {
		//logging interim state as read from the file
		jsonString, err := jsonutil.Marshal(commandState)
		if err != nil {
			log.Errorf("encountered error with message %v while marshalling %v to string", err, commandState)
		} else {
			log.Tracef("interim CommandState read from file-system - %v", jsonutil.Indent(jsonString))
		}
	}

	return commandState
}

// setDocState persists given commandState
func setDocState(log log.T, commandState model.DocumentState, absoluteFileName, locationFolder string) {

	content, err := jsonutil.Marshal(commandState)
	if err != nil {
		log.Errorf("encountered error with message %v while marshalling %v to string", err, commandState)
	} else {
		if fileutil.Exists(absoluteFileName) {
			log.Debugf("overwriting contents of %v", absoluteFileName)
		}
		log.Tracef("persisting interim state %v in file %v", jsonutil.Indent(content), absoluteFileName)
		if s, err := fileutil.WriteIntoFileWithPermissions(absoluteFileName, jsonutil.Indent(content), os.FileMode(int(appconfig.ReadWriteAccess))); s && err == nil {
			log.Debugf("successfully persisted interim state in %v", locationFolder)
		} else {
			log.Debugf("persisting interim state in %v failed with error %v", locationFolder, err)
		}
	}
}

// rLockDocument locks id specific RWMutex for reading
func rLockDocument(id string) {
	//check if document lock even exists
	if !doesLockExist(id) {
		createLock(id)
	}

	docLock[id].RLock()
}

// rUnlockDocument releases id specific single RLock
func rUnlockDocument(id string) {
	docLock[id].RUnlock()
}

// lockDocument locks id specific RWMutex for writing
func lockDocument(id string) {
	//check if document lock even exists
	if !doesLockExist(id) {
		createLock(id)
	}

	docLock[id].Lock()
}

// unlockDocument releases id specific Lock for writing
func unlockDocument(id string) {
	docLock[id].Unlock()
}

// doesLockExist returns true if there exists documentLock for given id
func doesLockExist(id string) bool {
	lock.RLock()
	defer lock.RUnlock()
	_, ok := docLock[id]
	return ok
}

// createLock creates id specific lock (RWMutex)
func createLock(id string) {
	lock.Lock()
	defer lock.Unlock()
	docLock[id] = &sync.RWMutex{}
}

// deleteLock deletes id specific lock
func deleteLock(id string) {
	lock.Lock()
	defer lock.Unlock()
	delete(docLock, id)
}

// docStateFileName returns absolute filename where command states are persisted
func docStateFileName(fileName, instanceID, locationFolder string) string {
	return path.Join(DocumentStateDir(instanceID, locationFolder), fileName)
}
