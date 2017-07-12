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

// Package processor implements MDS plugin processor
// processor_core contains functions that fetch messages and schedule them to be executed
package processor

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	commandStateHelper "github.com/aws/amazon-ssm-agent/agent/docmanager"
	"github.com/aws/amazon-ssm-agent/agent/docmanager/model"
	"github.com/aws/amazon-ssm-agent/agent/docparser"
	"github.com/aws/amazon-ssm-agent/agent/jsonutil"
	logger "github.com/aws/amazon-ssm-agent/agent/log"
	messageContracts "github.com/aws/amazon-ssm-agent/agent/message/contracts"
	"github.com/aws/amazon-ssm-agent/agent/message/processor/executer"
	"github.com/aws/amazon-ssm-agent/agent/message/service"
	"github.com/aws/amazon-ssm-agent/agent/platform"
	"github.com/aws/amazon-ssm-agent/agent/sdkutil"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/aws-sdk-go/service/ssmmds"
	"github.com/gabs"
)

var singletonMapOfUnsupportedSSMDocs map[string]bool
var once sync.Once

var loadDocStateFromSendCommand = parseSendCommandMessage
var loadDocStateFromCancelCommand = parseCancelCommandMessage

const (
	documentContent  = "DocumentContent"
	runtimeConfig    = "runtimeConfig"
	cloudwatchPlugin = "aws:cloudWatch"
	properties       = "properties"
	parameters       = "Parameters"
)

//TODO move this function to service module
func (p *Processor) processMessage(msg *ssmmds.Message) {
	var (
		docState *model.DocumentState
		err      error
	)

	// create separate logger that includes messageID with every log message
	context := p.context.With("[messageID=" + *msg.MessageId + "]")
	log := context.Log()
	log.Debug("Processing message")

	if err = validate(msg); err != nil {
		log.Error("message not valid, ignoring: ", err)
		return
	}

	if strings.HasPrefix(*msg.Topic, string(SendCommandTopicPrefix)) {
		docState, err = loadDocStateFromSendCommand(context, msg, p.orchestrationRootDir)
		if err != nil {
			log.Error(err)
			p.sendDocLevelResponse(*msg.MessageId, contracts.ResultStatusFailed, err.Error())
			return
		}
	} else if strings.HasPrefix(*msg.Topic, string(CancelCommandTopicPrefix)) {
		docState, err = loadDocStateFromCancelCommand(context, msg, p.orchestrationRootDir)
	} else {
		err = fmt.Errorf("unexpected topic name %v", *msg.Topic)
	}

	if err != nil {
		log.Error("format of received message is invalid ", err)
		if err = p.service.FailMessage(log, *msg.MessageId, service.InternalHandlerException); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
		return
	}

	//persisting received msg in file-system [pending folder]
	p.persistData(docState, appconfig.DefaultLocationOfPending)
	if err = p.service.AcknowledgeMessage(log, *msg.MessageId); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		return
	}

	log.Debugf("Ack done. Received message - messageId - %v", *msg.MessageId)

	log.Debugf("Processing to send a reply to update the document status to InProgress")

	//TODO This function should be called in service when it submits the document to the engine
	p.sendDocLevelResponse(*msg.MessageId, contracts.ResultStatusInProgress, "")

	log.Debugf("SendReply done. Received message - messageId - %v", *msg.MessageId)

	p.ExecutePendingDocument(docState)
}

// submitDocForExecution moves doc to current folder and submit it for execution
func (p *Processor) ExecutePendingDocument(docState *model.DocumentState) {
	log := p.context.Log()

	commandStateHelper.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfPending,
		appconfig.DefaultLocationOfCurrent)

	switch docState.DocumentType {
	case model.SendCommand, model.SendCommandOffline:
		//TODO Submit() needs to be an interface and job needs to be a document instead of callback
		err := p.sendCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processSendCommandMessage(
				p.context,
				p.service,
				cancelFlag,
				docState)
		})
		if err != nil {
			log.Error("SendCommand failed", err)
			return
		}

	case model.CancelCommand, model.CancelCommandOffline:
		err := p.cancelCommandPool.Submit(log, docState.DocumentInformation.MessageID, func(cancelFlag task.CancelFlag) {
			p.processCancelCommandMessage(p.context, p.service, p.sendCommandPool, docState)
		})
		if err != nil {
			log.Error("CancelCommand failed", err)
			return
		}

	default:
		log.Error("unexpected document type ", docState.DocumentType)
	}
}

//TODO move this function to service module
func parseSendCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*model.DocumentState, error) {
	log := context.Log()
	commandID := getCommandID(*msg.MessageId)

	log.Debug("Processing send command message ", *msg.MessageId)
	log.Trace("Processing send command message ", jsonutil.Indent(*msg.Payload))

	// parse message to retrieve parameters
	var parsedMessage messageContracts.SendCommandPayload
	err := json.Unmarshal([]byte(*msg.Payload), &parsedMessage)
	if err != nil {
		errorMsg := "Encountered error while parsing input - internal error"
		log.Errorf(errorMsg)
		return nil, fmt.Errorf("%v", errorMsg)
	}

	// adapt plugin configuration format from MDS to plugin expected format
	s3KeyPrefix := path.Join(parsedMessage.OutputS3KeyPrefix, parsedMessage.CommandID, *msg.Destination)

	messageOrchestrationDirectory := filepath.Join(messagesOrchestrationRootDir, commandID)

	var documentType model.DocumentType
	if strings.HasPrefix(*msg.Topic, string(SendCommandTopicPrefixOffline)) {
		documentType = model.SendCommandOffline
	} else {
		documentType = model.SendCommand
	}
	documentInfo := newDocumentInfo(*msg, parsedMessage)
	parserInfo := docparser.DocumentParserInfo{
		OrchestrationDir: messageOrchestrationDirectory,
		S3Bucket:         parsedMessage.OutputS3BucketName,
		S3Prefix:         s3KeyPrefix,
		MessageId:        documentInfo.MessageID,
		DocumentId:       documentInfo.DocumentID,
	}

	//Data format persisted in Current Folder is defined by the struct - CommandState
	docState, err := docparser.InitializeDocState(log, documentType, &parsedMessage.DocumentContent, documentInfo, parserInfo, parsedMessage.Parameters)
	if err != nil {
		return nil, err
	}
	parsedMessageContent, _ := jsonutil.Marshal(parsedMessage)

	var parsedContentJson *gabs.Container

	if parsedContentJson, err = gabs.ParseJSON([]byte(parsedMessageContent)); err != nil {
		log.Debugf("Parsed message is in the wrong json format. Error is ", err)
	}
	//Search for "DocumentContent" > "runtimeConfig" > "aws:cloudWatch" > "properties" which has the cloudwatch
	// config file and scrub the credentials, if present
	obj := parsedContentJson.Search(documentContent, runtimeConfig, cloudwatchPlugin, properties).String()
	if obj != "{}" {
		//This will be true only for aws:cloudwatch
		stripConfig := strings.Replace(strings.Replace(strings.Replace(obj, "\\t", "", -1), "\\n", "", -1), "\\", "", -1)
		stripConfig = strings.TrimSuffix(strings.TrimPrefix(stripConfig, "\""), "\"")

		finalLogConfig := logger.PrintCWConfig(stripConfig, log)

		// Parameters > properties is another path where the config file is printed
		if _, err = parsedContentJson.Set(finalLogConfig, parameters, properties); err != nil {
			log.Debug("Error occurred when setting Parameters->properties with scrubbed credentials - ", err)
		}
		if _, err = parsedContentJson.Set(finalLogConfig, documentContent, runtimeConfig, cloudwatchPlugin, properties); err != nil {
			log.Debug("Error occurred when setting aws:cloudWatch->properties with scrubbed credentials - ", err)
		}
		log.Debug("ParsedMessage is ", parsedContentJson.StringIndent("", "  "))
	} else {
		//For plugins that are not aws:cloudwatch
		log.Debug("ParsedMessage is ", jsonutil.Indent(parsedMessageContent))
	}
	// Check if it is a managed instance and its executing managed instance incompatible AWS SSM public document.
	// A few public AWS SSM documents contain code which is not compatible when run on managed instances.
	// isManagedInstanceIncompatibleAWSSSMDocument makes sure to find such documents at runtime and replace the incompatible code.
	isMI, err := platform.IsManagedInstance()
	if err != nil {
		log.Errorf("Error determining managed instance. error: %v", err)
	}

	if isMI && model.IsManagedInstanceIncompatibleAWSSSMDocument(docState.DocumentInformation.DocumentName) {
		log.Debugf("Running incompatible AWS SSM Document %v on managed instance", docState.DocumentInformation.DocumentName)
		if err = model.RemoveDependencyOnInstanceMetadata(context, &docState); err != nil {
			return nil, err
		}
	}

	return &docState, nil
}

// processSendCommandMessage processes a single send command message received from MDS.
func (p *Processor) processSendCommandMessage(context context.T,
	mdsService service.Service,
	cancelFlag task.CancelFlag,
	docState *model.DocumentState) {

	log := context.Log()

	documentID := docState.DocumentInformation.DocumentID
	instanceID := docState.DocumentInformation.InstanceID
	messageID := docState.DocumentInformation.MessageID
	//TODO This will be changed to a function (or channel) that hands the result over to service
	sendResponse := responseProvider(log, messageID, mdsService, p.config.AgentInfo, p.processorStopPolicy)
	//This should be the only allocation Processor has
	e := p.executerCreator(context)
	docStore := executer.NewDocumentFileStore(context, instanceID, documentID, appconfig.DefaultLocationOfCurrent, docState)
	log.Debug("Running executer...")
	resChan := e.Run(
		cancelFlag,
		&docStore,
	)

	for res := range resChan {
		log.Infof("sending reply for plugin %v update", res.LastPlugin)
		//TODO move this function and its bounded closure to service
		sendResponse(res.LastPlugin, res)

	}

	newCmdState := docStore.Load()
	// Skip move docState since the document has not finshed yet
	if newCmdState.DocumentInformation.DocumentStatus == contracts.ResultStatusSuccessAndReboot {
		log.Infof("document %v did not finish up execution, need to resume", newCmdState.DocumentInformation.MessageID)
		return
	}

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("execution of %v is over. Moving interimState file from Current to Completed folder", newCmdState.DocumentInformation.MessageID)

	commandStateHelper.MoveDocumentState(log,
		documentID,
		instanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")

	//TODO this part should be moved to service
	if !isUpdatePlugin(newCmdState) {
		if err := mdsService.DeleteMessage(log, messageID); err != nil {
			sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
		}
	} else {
		log.Debug("MessageDeletion skipped as it will be handled by external process")
	}
}

// processCancelCommandMessage processes a single send command message received from MDS.
//TODO this function is part of Processor's workflow, however it should eventually call into Executer to cancel a running document
func (p *Processor) processCancelCommandMessage(context context.T,
	mdsService service.Service,
	sendCommandPool task.Pool,
	docState *model.DocumentState) {

	log := context.Log()

	log.Debugf("Canceling job with id %v...", docState.CancelInformation.CancelMessageID)

	if found := sendCommandPool.Cancel(docState.CancelInformation.CancelMessageID); !found {
		log.Debugf("Job with id %v not found (possibly completed)", docState.CancelInformation.CancelMessageID)
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v couldn't be cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusFailed
	} else {
		docState.CancelInformation.DebugInfo = fmt.Sprintf("Command %v cancelled", docState.CancelInformation.CancelCommandID)
		docState.DocumentInformation.DocumentStatus = contracts.ResultStatusSuccess
	}

	//persist the final status of cancel-message in current folder
	commandStateHelper.PersistData(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent, docState)

	//persist : commands execution in completed folder (terminal state folder)
	log.Debugf("Execution of %v is over. Moving interimState file from Current to Completed folder", docState.DocumentInformation.MessageID)

	commandStateHelper.MoveDocumentState(log,
		docState.DocumentInformation.DocumentID,
		docState.DocumentInformation.InstanceID,
		appconfig.DefaultLocationOfCurrent,
		appconfig.DefaultLocationOfCompleted)

	log.Debugf("Deleting message")
	if err := mdsService.DeleteMessage(log, docState.DocumentInformation.MessageID); err != nil {
		sdkutil.HandleAwsError(log, err, p.processorStopPolicy)
	}
}

//TODO move this function to service module
func parseCancelCommandMessage(context context.T, msg *ssmmds.Message, messagesOrchestrationRootDir string) (*model.DocumentState, error) {
	log := context.Log()

	log.Debug("Processing cancel command message - ", *msg.MessageId)

	var parsedMessage messageContracts.CancelPayload
	err := json.Unmarshal([]byte(*msg.Payload), &parsedMessage)
	if err != nil {
		return nil, err
	}

	//persist in current folder here
	docState := initializeCancelCommandState(*msg, parsedMessage)
	return &docState, nil
}

//TODO move this to service, this is just used in check for message deletion
func isUpdatePlugin(pluginConfig *model.DocumentState) bool {
	for _, pluginState := range pluginConfig.InstancePluginsInformation {
		if pluginState.Name == appconfig.PluginEC2ConfigUpdate || pluginState.Name == appconfig.PluginNameAwsAgentUpdate {
			return true
		}
	}
	return false
}
