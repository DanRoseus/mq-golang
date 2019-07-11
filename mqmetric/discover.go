/*
Package mqmetric contains a set of routines common to several
commands used to export MQ metrics to different backend
storage mechanisms including Prometheus and InfluxDB.
*/
package mqmetric

/*
  Copyright (c) IBM Corporation 2016, 2019

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.

   Contributors:
     Mark Taylor - Initial Contribution
*/

/*
Functions in this file discover the data available from a queue manager
via the MQ V9 pub/sub monitoring feature. Each metric (element) is
found by discovering the types of metric, and the types are found by first
discovering the classes. Sample program amqsrua is shipped with MQ V9 to
give a good demonstration of the process, which is followed here.
*/

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/ibm-messaging/mq-golang/ibmmq"
)

// MonElement describes the real metric element generated by MQ
type MonElement struct {
	Parent         *MonType
	Description    string // An English phrase describing the element
	DescriptionNLS string // A translated phrase for the current locale
	MetricName     string // Reformatted description suitable as label
	Datatype       int32
	Values         map[string]int64
}

// MonType describes the "types" of data generated by MQ. Each class generates
// one or more type of data such as OPENCLOSE (from STATMQI class) or
// LOG (from DISK class)
type MonType struct {
	Parent       *MonClass
	Name         string
	Description  string
	ObjectTopic  string // topic for actual data responses
	elementTopic string // discovery of elements
	Elements     map[int]*MonElement
	subHobj      map[string]ibmmq.MQObject
}

// MonClass described the "classes" of data generated by MQ, such as DISK and CPU
type MonClass struct {
	Parent      *AllMetrics
	Name        string
	Description string
	typesTopic  string
	flags       int
	Types       map[int]*MonType
}

// The AllMetrics structure is the top of the tree, holding the set of classes.
type AllMetrics struct {
	Classes map[int]*MonClass
}

type QInfo struct {
	MaxDepth        int64
	Usage           int64
	exists          bool // Used during rediscovery
	firstCollection bool // To indicate discard needed of first stat
}

// QMgrMapKey can never be a real object name and is therefore useful in
// maps that may contain only this single entry
const QMgrMapKey = "@self"

const maxBufSize = 100 * 1024 * 1024 // 100 MB
const defaultMaxQDepth = 5000

// Metrics is the global variable for the tree of data
var Metrics AllMetrics

var qInfoMap map[string]*QInfo
var locale string
var discoveryDone = false

func GetDiscoveredQueues() []string {
	keys := make([]string, 0)
	for key := range qInfoMap {
		keys = append(keys, key)
	}
	return keys
}

/*
 * A collector can set the locale (eg "Fr_FR") before doing the discovery
 * process to get access to the MQ-translated strings
 */
func SetLocale(l string) {
	locale = l
}

/*
 * Check any important parameters  - this must be called after DiscoverAndSubscribe
 * to maintain compatibility of the package's APIs.  It also needs the list of queues to have been
 * populated first which is also done in DiscoverAndSubscribe.
 * Returns: an MQ CompCode, error string. CompCode can be MQCC_OK, WARNING or ERROR.
 */
func VerifyConfig() (int32, error) {
	var err error
	var v map[int32]interface{}
	var compCode = ibmmq.MQCC_OK
	if !discoveryDone {
		err = fmt.Errorf("Error: Need to call DiscoverAndSubscribe first")
		compCode = ibmmq.MQCC_FAILED
	}

	if err == nil {
		selectors := []int32{ibmmq.MQIA_MAX_Q_DEPTH, ibmmq.MQIA_DEFINITION_TYPE}
		v, err = replyQObj.InqMap(selectors)
		if err == nil {
			maxQDepth := v[ibmmq.MQIA_MAX_Q_DEPTH].(int32)
			// Function has tuning based on number of queues to be monitored
			// Current published resource topics are approx 16 subs for 95 elements on the qmgr
			// ... and 35 elements per queue in 4 subs
			// Round these to 20 and 5 for a bit of headroom
			// Make recommended minimum qdepth  60 / 10 * total per interval to allow one minute of data
			// as MQ publications are at 10 second interval by default (and no public tuning)
			// and assume monitor collection interval is one minute
			// Since we don't do pubsub-based collection on z/OS, this qdepth doesn't matter
			recommendedDepth := (20 + len(qInfoMap)*5) * 6
			if maxQDepth < int32(recommendedDepth) && usePublications {
				err = fmt.Errorf("Warning: Maximum queue depth on %s may be too low. Current value = %d", replyQBaseName, maxQDepth)
				compCode = ibmmq.MQCC_WARNING
			}

			// Make sure this reply queue that has been opened is not a predefined queue, so it
			// has come from a model definition. The base replyQ is opened twice for different reasons.
			// A LOCAL queue would end up with mixed sets of replies/publications
			defType := v[ibmmq.MQIA_DEFINITION_TYPE].(int32)
			if defType == ibmmq.MQQDT_PREDEFINED {
				err = fmt.Errorf("Error: ReplyQ parameter %s must refer to a MODEL queue,", replyQBaseName)
				compCode = ibmmq.MQCC_FAILED
			}
		}
	}
	return compCode, err
}

/*
DiscoverAndSubscribe does the work of finding the
different resources available from a queue manager and
issuing the MQSUB calls to collect the data
*/
func DiscoverAndSubscribe(queueList string, checkQueueList bool, metaPrefix string) error {
	discoveryDone = true
	redo := false
	qInfoMap = make(map[string]*QInfo)

	err := discoverAndSubscribe(queueList, checkQueueList, metaPrefix, redo)
	return err
}
func RediscoverAndSubscribe(queueList string, checkQueueList bool, metaPrefix string) error {
	discoveryDone = true
	redo := true

	// Assume queues have been deleted and we will tidy up later.
	// The flag is reset to true during the discovery process if the queue still exists
	for _, qi := range qInfoMap {
		qi.exists = false
	}

	err := discoverAndSubscribe(queueList, checkQueueList, metaPrefix, redo)

	// We now know if a queue still exists; remove it from the map if not.
	for key, qi := range qInfoMap {
		if !qi.exists {
			delete(qInfoMap, key)
		}
	}
	return err
}

func discoverAndSubscribe(queueList string, checkQueueList bool, metaPrefix string, redo bool) error {
	var err error

	// What metrics can the queue manager provide?
	if err == nil && redo == false {
		err = discoverStats(metaPrefix)
	}

	// Which queues have we been asked to monitor? Expand wildcards
	// to explicit names so that subscriptions work.
	if err == nil {
		if checkQueueList {
			err = discoverQueues(queueList)
		} else {
			qList := strings.Split(queueList, ",")
			// Make sure the names are reasonably valid
			for i := 0; i < len(qList); i++ {
				key := strings.TrimSpace(qList[i])
				qInfoMap[key] = new(QInfo)
			}
		}

	}

	// Subscribe to all of the various topics
	if err == nil {
		err = createSubscriptions()
	}

	return err
}

func discoverClasses(metaPrefix string) error {
	var data []byte
	var sub ibmmq.MQObject
	var metaReplyQObj ibmmq.MQObject
	var err error
	var rootTopic string

	// Have to know the starting point for the topic that tells about classes
	if metaPrefix == "" {
		rootTopic = "$SYS/MQ/INFO/QMGR/" + resolvedQMgrName + "/Monitor/METADATA/CLASSES"
	} else {
		rootTopic = metaPrefix + "/INFO/QMGR/" + resolvedQMgrName + "/Monitor/METADATA/CLASSES"
	}
	sub, err = subscribeManaged(rootTopic, &metaReplyQObj)
	if err == nil {
		data, err = getMessageWithHObj(true, metaReplyQObj)
		defer sub.Close(0)

		elemList, _ := parsePCFResponse(data)

		for i := 0; i < len(elemList); i++ {
			if elemList[i].Type != ibmmq.MQCFT_GROUP {
				continue
			}
			group := elemList[i]
			cl := new(MonClass)
			classIndex := 0
			cl.Types = make(map[int]*MonType)
			cl.Parent = &Metrics

			for j := 0; j < len(group.GroupList); j++ {
				elem := group.GroupList[j]
				switch elem.Parameter {
				case ibmmq.MQIAMO_MONITOR_CLASS:
					classIndex = int(elem.Int64Value[0])
				case ibmmq.MQIAMO_MONITOR_FLAGS:
					cl.flags = int(elem.Int64Value[0])
				case ibmmq.MQCAMO_MONITOR_CLASS:
					cl.Name = elem.String[0]
				case ibmmq.MQCAMO_MONITOR_DESC:
					cl.Description = elem.String[0]
				case ibmmq.MQCA_TOPIC_STRING:
					cl.typesTopic = elem.String[0]
				default:
					return fmt.Errorf("Unknown parameter %d in class discovery", elem.Parameter)
				}
			}
			Metrics.Classes[classIndex] = cl
		}
	}

	subsOpened = true
	return err
}

func discoverTypes(cl *MonClass) error {
	var data []byte
	var sub ibmmq.MQObject
	var metaReplyQObj ibmmq.MQObject
	var err error

	sub, err = subscribeManaged(cl.typesTopic, &metaReplyQObj)
	if err == nil {
		data, err = getMessageWithHObj(true, metaReplyQObj)
		defer sub.Close(0)

		elemList, _ := parsePCFResponse(data)

		for i := 0; i < len(elemList); i++ {
			if elemList[i].Type != ibmmq.MQCFT_GROUP {
				continue
			}

			group := elemList[i]
			ty := new(MonType)
			ty.Elements = make(map[int]*MonElement)
			ty.subHobj = make(map[string]ibmmq.MQObject)

			typeIndex := 0
			ty.Parent = cl

			for j := 0; j < len(group.GroupList); j++ {
				elem := group.GroupList[j]
				switch elem.Parameter {

				case ibmmq.MQIAMO_MONITOR_TYPE:
					typeIndex = int(elem.Int64Value[0])
				case ibmmq.MQCAMO_MONITOR_TYPE:
					ty.Name = elem.String[0]
				case ibmmq.MQCAMO_MONITOR_DESC:
					ty.Description = elem.String[0]
				case ibmmq.MQCA_TOPIC_STRING:
					ty.elementTopic = elem.String[0]
				default:
					return fmt.Errorf("Unknown parameter %d in type discovery", elem.Parameter)
				}
			}
			cl.Types[typeIndex] = ty
		}
	}
	return err
}

func discoverElements(ty *MonType) error {
	var err error
	var data []byte
	var sub ibmmq.MQObject
	var metaReplyQObj ibmmq.MQObject
	var elem *MonElement

	sub, err = subscribeManaged(ty.elementTopic, &metaReplyQObj)
	if err == nil {
		data, err = getMessageWithHObj(true, metaReplyQObj)
		defer sub.Close(0)

		elemList, _ := parsePCFResponse(data)

		for i := 0; i < len(elemList); i++ {

			if elemList[i].Type == ibmmq.MQCFT_STRING && elemList[i].Parameter == ibmmq.MQCA_TOPIC_STRING {
				ty.ObjectTopic = elemList[i].String[0]
				continue
			}

			if elemList[i].Type != ibmmq.MQCFT_GROUP {
				continue
			}

			group := elemList[i]

			elem = new(MonElement)
			elementIndex := 0
			elem.Parent = ty
			elem.Values = make(map[string]int64)

			for j := 0; j < len(group.GroupList); j++ {
				e := group.GroupList[j]
				switch e.Parameter {

				case ibmmq.MQIAMO_MONITOR_ELEMENT:
					elementIndex = int(e.Int64Value[0])
				case ibmmq.MQIAMO_MONITOR_DATATYPE:
					elem.Datatype = int32(e.Int64Value[0])
				case ibmmq.MQCAMO_MONITOR_DESC:
					elem.Description = e.String[0]
				default:
					return fmt.Errorf("Unknown parameter %d in type discovery", e.Parameter)
				}
			}

			elem.MetricName = formatDescription(elem)
			ty.Elements[elementIndex] = elem
		}
	}

	return err
}

// Rerun the subscription for elements, but this time adding a locale into the topic
// so that we can get the translated description. It's up to the collector program to
// then make use of that description.
func discoverElementsNLS(ty *MonType, locale string) error {
	var err error
	var data []byte
	var sub ibmmq.MQObject
	var metaReplyQObj ibmmq.MQObject

	if locale == "" {
		return nil
	}

	sub, err = subscribe(ty.elementTopic+"/"+locale, &metaReplyQObj)
	if err == nil {
		// Don't wait - if there's nothing on that topic, then get out fast
		data, err = getMessageWithHObj(false, metaReplyQObj)
		sub.Close(0)

		if err != nil {
			mqreturn := err.(*ibmmq.MQReturn)
			if mqreturn.MQRC == ibmmq.MQRC_NO_MSG_AVAILABLE {
				err = nil
			}
		}

		elemList, _ := parsePCFResponse(data)

		for i := 0; i < len(elemList); i++ {

			if elemList[i].Type == ibmmq.MQCFT_STRING && elemList[i].Parameter == ibmmq.MQCA_TOPIC_STRING {
				continue
			}

			if elemList[i].Type != ibmmq.MQCFT_GROUP {
				continue
			}

			group := elemList[i]
			description := ""
			elementIndex := 0

			for j := 0; j < len(group.GroupList); j++ {
				e := group.GroupList[j]
				switch e.Parameter {

				case ibmmq.MQIAMO_MONITOR_ELEMENT:
					elementIndex = int(e.Int64Value[0])

				case ibmmq.MQCAMO_MONITOR_DESC:
					description = e.String[0]

				}
			}

			if description != "" {
				ty.Elements[elementIndex].DescriptionNLS = description
			}
		}
	}

	return err
}

/*
Discover the complete set of available statistics in the queue manager
by working through the classes, types and individual elements.

Then discover the list of individual queues we have been asked for.
*/
func discoverStats(metaPrefix string) error {
	var err error

	// Start with an empty set of information about the available stats
	Metrics.Classes = make(map[int]*MonClass)

	// Allow us to proceed on z/OS even though it does not support pub/sub resources
	if metaPrefix == "" && !usePublications {
		return nil
	}

	// Then get the list of CLASSES
	err = discoverClasses(metaPrefix)

	// For each CLASS, discover the TYPEs of data available
	if err == nil {
		for _, cl := range Metrics.Classes {
			err = discoverTypes(cl)
			// And for each CLASS, discover the actual statistics elements
			if err == nil {
				for _, ty := range cl.Types {
					err = discoverElements(ty)
					if err == nil && locale != "" {
						err = discoverElementsNLS(ty, locale)
					}
				}
			}
		}

		// Validate all discovered metric names are unique
		// Need to add in if it's qmgr or q level
		nameSet := make(map[string]struct{})
		var exists = struct{}{}
		for _, cl := range Metrics.Classes {
			for _, ty := range cl.Types {
				for _, elem := range ty.Elements {
					name := elem.MetricName
					if strings.Contains(ty.ObjectTopic, "%s") {
						name = "object_" + name
					}
					if _, ok := nameSet[name]; ok {
						err = fmt.Errorf("Non-unique metric description '%s'", elem.MetricName)
					} else {
						nameSet[name] = exists
					}
				}
			}
		}

	}

	return err
}

/*
discoverQueues lists the queues that match all of the configured
patterns.

The patterns must match the MQ rule - asterisk on the end of the
string only.

If a bad pattern is used, or no queues exist that match the pattern
then an error is reported but we continue processing other patterns.

An alternative would be to list ALL the queues (though that could be a long list),
and then use a more general regexp match. Something for a later update perhaps.
*/
func discoverQueues(monitoredQueuePatterns string) error {
	var err error
	var qList []string
	var allQueues []string
	usingRegExp := false

	// If the list of monitored queues has a ! somewhere in it, we will
	// get the full list of queues on the qmgr, and filter it by patterns.
	if strings.Contains(monitoredQueuePatterns, "!") {
		usingRegExp = true
	}

	// A valid pattern list looks like
	//    !A*, !SYSTEM*, B*, DEV.QUEUE.1
	// If we know there are no exclusion patterns, then use the
	// set directly as it is more efficient
	if usingRegExp {
		allQueues, err = inquireObjects("*", ibmmq.MQOT_Q)
		if err == nil {
			qList = FilterRegExp(monitoredQueuePatterns, allQueues)
		}
	} else {
		qList, err = inquireObjects(monitoredQueuePatterns, ibmmq.MQOT_Q)
	}

	if len(qList) > 0 {
		//fmt.Printf("Monitoring Queues: %v\n", qList)
		for i := 0; i < len(qList); i++ {
			var qInfoElem *QInfo
			var ok bool
			qName := strings.TrimSpace(qList[i])
			if qInfoElem, ok = qInfoMap[qName]; !ok {
				qInfoElem = new(QInfo)
			}
			qInfoElem.MaxDepth = defaultMaxQDepth
			qInfoElem.exists = true
			qInfoMap[qName] = qInfoElem
		}

		if useStatus {
			if usingRegExp {
				for qName, _ := range qInfoMap {
					if len(qName) > 0 {
						inquireQueueAttributes(qName)
					}
				}
			} else {
				inquireQueueAttributes(monitoredQueuePatterns)
			}
		}

		if err != nil {
			//fmt.Printf("Queue Discovery Error: %v\n", err)
		}
		return nil
	}
	return err
}

func inquireObjects(objectPatternsList string, objectType int32) ([]string, error) {
	var err error
	var elem *ibmmq.PCFParameter
	var datalen int
	var objectList []string
	var command int32
	var attribute int32
	var returnedAttribute int32
	var missingPatterns string

	objectList = make([]string, 0)

	if objectPatternsList == "" {
		return nil, err
	}

	statusClearReplyQ()

	objectPatterns := strings.Split(strings.TrimSpace(objectPatternsList), ",")
	for i := 0; i < len(objectPatterns) && err == nil; i++ {
		var buf []byte
		pattern := strings.TrimSpace(objectPatterns[i])
		if len(pattern) == 0 {
			continue
		}

		if strings.Count(pattern, "*") > 1 ||
			(strings.Count(pattern, "*") == 1 && !strings.HasSuffix(pattern, "*")) {
			return nil, fmt.Errorf("Object pattern '%s' is not valid", pattern)
		}

		switch objectType {
		case ibmmq.MQOT_Q:
			command = ibmmq.MQCMD_INQUIRE_Q_NAMES
			attribute = ibmmq.MQCA_Q_NAME
			returnedAttribute = ibmmq.MQCACF_Q_NAMES
		case ibmmq.MQOT_CHANNEL:
			command = ibmmq.MQCMD_INQUIRE_CHANNEL_NAMES
			attribute = ibmmq.MQCACH_CHANNEL_NAME
			returnedAttribute = ibmmq.MQCACH_CHANNEL_NAMES
		default:
			return nil, fmt.Errorf("Object type %d is not valid", objectType)
		}

		putmqmd := ibmmq.NewMQMD()
		pmo := ibmmq.NewMQPMO()

		pmo.Options = ibmmq.MQPMO_NO_SYNCPOINT
		pmo.Options |= ibmmq.MQPMO_NEW_MSG_ID
		pmo.Options |= ibmmq.MQPMO_NEW_CORREL_ID
		pmo.Options |= ibmmq.MQPMO_FAIL_IF_QUIESCING

		putmqmd.Format = "MQADMIN"
		putmqmd.ReplyToQ = statusReplyQObj.Name
		putmqmd.MsgType = ibmmq.MQMT_REQUEST
		putmqmd.Report = ibmmq.MQRO_PASS_DISCARD_AND_EXPIRY

		cfh := ibmmq.NewMQCFH()
		cfh.Version = ibmmq.MQCFH_VERSION_3
		cfh.Type = ibmmq.MQCFT_COMMAND_XR

		// Can allow all the other fields to default
		cfh.Command = command

		// Add the parameters one at a time into a buffer
		pcfparm := new(ibmmq.PCFParameter)
		pcfparm.Type = ibmmq.MQCFT_STRING
		pcfparm.Parameter = attribute
		pcfparm.String = []string{pattern}
		cfh.ParameterCount++
		buf = append(buf, pcfparm.Bytes()...)

		if command == ibmmq.MQCMD_INQUIRE_Q_NAMES {
			pcfparm = new(ibmmq.PCFParameter)
			pcfparm.Type = ibmmq.MQCFT_INTEGER
			pcfparm.Parameter = ibmmq.MQIA_Q_TYPE
			pcfparm.Int64Value = []int64{int64(ibmmq.MQQT_LOCAL)}
			cfh.ParameterCount++
			buf = append(buf, pcfparm.Bytes()...)
		}
		// Once we know the total number of parameters, put the
		// CFH header on the front of the buffer.
		buf = append(cfh.Bytes(), buf...)

		// And put the command to the queue
		err = cmdQObj.Put(putmqmd, pmo, buf)

		if err != nil {
			return objectList, err
		}

		// Now get the response
		getmqmd := ibmmq.NewMQMD()
		gmo := ibmmq.NewMQGMO()
		gmo.Options = ibmmq.MQGMO_NO_SYNCPOINT
		gmo.Options |= ibmmq.MQGMO_FAIL_IF_QUIESCING
		gmo.Options |= ibmmq.MQGMO_WAIT
		gmo.Options |= ibmmq.MQGMO_CONVERT
		gmo.WaitInterval = 30 * 1000

		// Pick a default buffer size but allow it to double on retries to cope with
		// truncated messages.
		bufSize := 32768

		for truncation := true; truncation; {
			buf = make([]byte, bufSize)

			datalen, err = statusReplyQObj.Get(getmqmd, gmo, buf)
			if err == nil {
				truncation = false
				cfh, offset := ibmmq.ReadPCFHeader(buf)
				if cfh.CompCode != ibmmq.MQCC_OK {
					return objectList, fmt.Errorf("PCF command failed with CC %s [%d] RC %s [%d]",
						ibmmq.MQItoString("CC", int(cfh.CompCode)), cfh.CompCode,
						ibmmq.MQItoString("RC", int(cfh.Reason)), cfh.Reason)
				} else {
					parmAvail := true
					bytesRead := 0
					if cfh.ParameterCount == 0 {
						parmAvail = false
						missingPatterns = missingPatterns + " " + pattern
					}

					for parmAvail && cfh.CompCode != ibmmq.MQCC_FAILED {
						elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
						offset += bytesRead
						// Have we now reached the end of the message
						if offset >= datalen {
							parmAvail = false
						}

						switch elem.Parameter {
						case returnedAttribute:
							if len(elem.String) == 0 {
								missingPatterns = missingPatterns + " " + pattern
							}
							for i := 0; i < len(elem.String); i++ {
								objectList = append(objectList, strings.TrimSpace(elem.String[i]))
							}
						}
					}
				}
			} else {
				mqreturn := err.(*ibmmq.MQReturn)
				if mqreturn.MQCC != ibmmq.MQCC_OK && mqreturn.MQRC == ibmmq.MQRC_TRUNCATED_MSG_FAILED && bufSize <= maxBufSize {
					truncation = true
					bufSize *= 2
					if bufSize > maxBufSize {
						bufSize = maxBufSize
					}
				} else {
					return objectList, err
				}
			}
		}
	}

	if len(missingPatterns) > 0 && err == nil {
		err = fmt.Errorf("No objects matching %s of type %d exist", missingPatterns, objectType)
	}
	return objectList, err
}

/*
Now that we know which topics can return data, need to
create all the subscriptions.
*/
func createSubscriptions() error {
	var err error
	var sub ibmmq.MQObject
	for _, cl := range Metrics.Classes {
		for _, ty := range cl.Types {

			if strings.Contains(ty.ObjectTopic, "%s") {
				for key, _ := range qInfoMap {
					if len(key) == 0 {
						continue
					}

					// See if we've already got a subscription
					// for this object
					if s, ok := ty.subHobj[key]; ok {
						if qInfoMap[key].exists {
							// leave alone
						} else {
							s.Close(0)
							delete(ty.subHobj, key)
						}
					} else {
						topic := fmt.Sprintf(ty.ObjectTopic, key)
						sub, err = subscribe(topic, &replyQObj)
						if err == nil {
							ty.subHobj[key] = sub
							qInfoMap[key].firstCollection = true
						}
					}
				}
			} else {
				if _, ok := ty.subHobj[QMgrMapKey]; !ok {

					// Don't have a qmgr-level subscription to this topic. Should
					// only do this subscription once at startup
					sub, err = subscribe(ty.ObjectTopic, &replyQObj)
					ty.subHobj[QMgrMapKey] = sub
				}
			}

			if err != nil {
				return fmt.Errorf("Error subscribing to %s: %v", ty.ObjectTopic, err)
			}
		}
	}

	return err
}

/*
ProcessPublications has to read all of the messages since the last scrape
and update the values for every relevant gauge.

Because the generation of the messages by the qmgr, and being told to
read them by the main loop, may not have identical frequencies, there may be
cases where multiple pieces of data have to be collated for the same
gauge. Conversely, there may be times when this is called but there
are no metrics to update.
*/
func ProcessPublications() error {
	var err error
	var data []byte

	var qName string
	var classidx int
	var typeidx int
	var elementidx int
	var value int64

	if !usePublications {
		return nil
	}

	// Keep reading all available messages until queue is empty. Don't
	// do a GET-WAIT; just immediate removals.
	cnt := 0
	for err == nil {
		data, err = getMessage(false)

		// Most common error will be MQRC_NO_MESSAGE_AVAILABLE
		// which will end the loop.
		if err == nil {
			cnt++
			elemList, _ := parsePCFResponse(data)

			// A typical publication contains some fixed
			// headers (qmgrName, objectName, class, type etc)
			// followed by a list of index/values.

			// This map contains those element indexes and values from each message
			values := make(map[int]int64)

			qName = ""

			for i := 0; i < len(elemList); i++ {
				switch elemList[i].Parameter {
				case ibmmq.MQCA_Q_MGR_NAME:
					_ = strings.TrimSpace(elemList[i].String[0])
				case ibmmq.MQCA_Q_NAME:
					qName = strings.TrimSpace(elemList[i].String[0])
				case ibmmq.MQCA_TOPIC_NAME:
					qName = strings.TrimSpace(elemList[i].String[0])
				case ibmmq.MQIACF_OBJECT_TYPE:
					// Will need to use this as part of the object key and
					// labelling if/when MQ starts to produce stats for other types
					// such as a topic. But for now we can ignore it.
					_ = ibmmq.MQItoString("OT", int(elemList[i].Int64Value[0]))
				case ibmmq.MQIAMO_MONITOR_CLASS:
					classidx = int(elemList[i].Int64Value[0])
				case ibmmq.MQIAMO_MONITOR_TYPE:
					typeidx = int(elemList[i].Int64Value[0])
				case ibmmq.MQIAMO64_MONITOR_INTERVAL:
					_ = elemList[i].Int64Value[0]
				case ibmmq.MQIAMO_MONITOR_FLAGS:
					_ = int(elemList[i].Int64Value[0])
				default:
					value = elemList[i].Int64Value[0]
					elementidx = int(elemList[i].Parameter)
					values[elementidx] = value
				}
			}

			// Now have all the values in this particular message
			// Have to incorporate them into any that already exist.
			//
			// Each element contains a map holding all the objects
			// touched by these messages. The map is referenced by
			// object name if it's a queue; for qmgr-level stats, the
			// map only needs to contain a single entry which I've
			// chosen to reference by "@self" which can never be a
			// real queue name.
			//
			// We have to know whether to need to add the values
			// contained from multiple publications that might
			// have arrived in the scrape interval
			// for the same resource, or whether we should just
			// overwrite with the latest. Although there are
			// several monitor Datatypes, all of them apart from
			// explicitly labelled "DELTA" are ones we should just
			// use the latest value.
			for key, newValue := range values {
				if elem, ok := Metrics.Classes[classidx].Types[typeidx].Elements[key]; ok {
					objectName := qName

					if objectName == "" {
						objectName = QMgrMapKey
					} else {
						// If we've unsubscribed and resubscribed to the same queue (unusual
						// but a dynamic resub nature may permit that) then discard the first metric
						// from a queue in case it's got a running total instead of the last interval.
						if qi, ok := qInfoMap[qName]; ok {
							if qi.firstCollection {
								continue
							}
							if !qi.exists {
								//fmt.Printf("Data for untracked queue %s being ignored\n", qName)
								continue
							}
						} else {
							//fmt.Printf("Data for unknown queue %s being ignored\n", qName)
							continue
						}
					}

					if oldValue, ok := elem.Values[objectName]; ok {
						if elem.Datatype == ibmmq.MQIAMO_MONITOR_DELTA {
							value = oldValue + newValue
						} else {
							value = newValue
						}
					} else {
						value = newValue
					}
					elem.Values[objectName] = value
				}
			}
		} else {
			// err != nil
			mqreturn := err.(*ibmmq.MQReturn)

			if mqreturn.MQCC == ibmmq.MQCC_FAILED && mqreturn.MQRC != ibmmq.MQRC_NO_MSG_AVAILABLE {
				return mqreturn
			}
		}
	}

	// Ensure that all known queues are marked as having had at least one collection cycle
	for _, qi := range qInfoMap {
		qi.firstCollection = false
	}
	return nil
}

/*
Parse a PCF response message, returning the
elements. If an element represents a PCF group, that element
has the pieces of the group attached to itself. While
it is theoretically possible for groups to contain groups, MQ never
does that, so the code here does not need to recurse through multiple
levels.

Returns TRUE if this is the last response in a
set, based on the MQCFH.Control value.
*/
func parsePCFResponse(buf []byte) ([]*ibmmq.PCFParameter, bool) {
	var elem *ibmmq.PCFParameter
	var elemList []*ibmmq.PCFParameter
	var bytesRead int

	rc := false

	// First get the MQCFH structure. This also returns
	// the number of bytes read so we know where to start
	// looking for the next element
	cfh, offset := ibmmq.ReadPCFHeader(buf)

	// If the command succeeded, loop through the remainder of the
	// message to decode each parameter.
	for i := 0; i < int(cfh.ParameterCount); i++ {
		// We don't know how long the parameter is, so we just
		// pass in "from here to the end" and let the parser
		// tell us how far it got.
		elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
		offset += bytesRead
		// Have we now reached the end of the message
		elemList = append(elemList, elem)
		if elem.Type == ibmmq.MQCFT_GROUP {
			groupElem := elem
			for j := 0; j < int(groupElem.ParameterCount); j++ {
				elem, bytesRead = ibmmq.ReadPCFParameter(buf[offset:])
				offset += bytesRead
				groupElem.GroupList = append(groupElem.GroupList, elem)
			}
		}

	}

	if cfh.Control == ibmmq.MQCFC_LAST {
		rc = true
	}
	return elemList, rc
}

/*
Need to turn the "friendly" name of each element into something
that is suitable for metric names.

Should also have consistency of units (always use seconds,
bytes etc), and organisation of the elements of the name (units last)

While we can't change the MQ-generated descriptions for its statistics,
we can reformat most of them heuristically here.
*/
func formatDescription(elem *MonElement) string {
	s := elem.Description
	s = strings.Replace(s, " ", "_", -1)
	s = strings.Replace(s, "/", "_", -1)
	s = strings.Replace(s, "-", "_", -1)

	/* Make sure we don't have multiple underscores */
	multiunder := regexp.MustCompile("__*")
	s = multiunder.ReplaceAllLiteralString(s, "_")

	/* make it all lowercase. Not essential, but looks better */
	s = strings.ToLower(s)

	/* Remove all cases of bytes, seconds, count or percentage (we add them back in later) */
	s = strings.Replace(s, "_count", "", -1)
	s = strings.Replace(s, "_bytes", "", -1)
	s = strings.Replace(s, "_byte", "", -1)
	s = strings.Replace(s, "_seconds", "", -1)
	s = strings.Replace(s, "_second", "", -1)
	s = strings.Replace(s, "_percentage", "", -1)

	// Switch round a couple of specific names
	s = strings.Replace(s, "messages_expired", "expired_messages", -1)

	// Add the unit at end
	switch elem.Datatype {
	case ibmmq.MQIAMO_MONITOR_PERCENT, ibmmq.MQIAMO_MONITOR_HUNDREDTHS:
		s = s + "_percentage"
	case ibmmq.MQIAMO_MONITOR_MB, ibmmq.MQIAMO_MONITOR_GB:
		s = s + "_bytes"
	case ibmmq.MQIAMO_MONITOR_MICROSEC:
		s = s + "_seconds"
	default:
		if strings.Contains(s, "_total") {
			/* If we specify it is a total in description put that at the end */
			s = strings.Replace(s, "_total", "", -1)
			s = s + "_total"
		} else if strings.Contains(s, "log_") {
			/* Weird case where the log datatype is not MB or GB but should be bytes */
			s = s + "_bytes"
		}

		// There are some metrics that have both "count" and "byte count" in
		// the descriptions. They were getting mapped to the same string, so
		// we have to ensure uniqueness.
		if strings.Contains(elem.Description, "byte count") {
			s = s + "_bytes"
		} else if strings.HasSuffix(elem.Description, " count") && !strings.Contains(s, "_count") {
			s = s + "_count"
		}
	}

	return s
}

/*
ReadPatterns is called during the initial configuration step to read a file
containing object name patterns if they are not explicitly given
on the command line.
*/
func ReadPatterns(f string) (string, error) {
	var s string

	file, err := os.Open(f)
	if err != nil {
		return "", fmt.Errorf("Error Opening file %s: %v", f, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if s != "" {
			s += ","
		}
		s += scanner.Text()
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("Error Reading from %s: %v", f, err)
	}

	return s, nil
}

/*
Normalise converts the value returned from MQ into the correct units
such as converting MB to bytes.
*/
func Normalise(elem *MonElement, key string, value int64) float64 {
	f := float64(value)
	// I've  seen negative numbers which are nonsense,
	// possibly 32-bit overflow or uninitialised values
	// in the qmgr. So force data to something sensible
	// just in case those were due to a bug.
	if f < 0 {
		f = 0
	}

	// Convert suitable metrics to base units
	if elem.Datatype == ibmmq.MQIAMO_MONITOR_PERCENT ||
		elem.Datatype == ibmmq.MQIAMO_MONITOR_HUNDREDTHS {
		f = f / 100
	} else if elem.Datatype == ibmmq.MQIAMO_MONITOR_MB {
		f = f * 1024 * 1024
	} else if elem.Datatype == ibmmq.MQIAMO_MONITOR_GB {
		f = f * 1024 * 1024 * 1024
	} else if elem.Datatype ==
		ibmmq.MQIAMO_MONITOR_MICROSEC {
		f = f / 1000000
	}

	return f
}

func VerifyPatterns(patternList string) error {
	return verifyObjectPatterns(patternList, false)
}
func VerifyQueuePatterns(patternList string) error {
	return verifyObjectPatterns(patternList, true)
}
func verifyObjectPatterns(patternList string, allowNegatives bool) error {
	var err error
	objectPatterns := strings.Split(patternList, ",")
	for i := 0; i < len(objectPatterns) && err == nil; i++ {

		pattern := strings.TrimSpace(objectPatterns[i])
		if pattern == "" {
			continue
		}
		if strings.Count(pattern, "*") > 1 ||
			(strings.Count(pattern, "*") == 1 && !strings.HasSuffix(pattern, "*")) {
			err = fmt.Errorf("Object pattern '%s' is not valid. '*' must be last character in a pattern", pattern)
		}
		// Will allow ! to be at the start of a pattern.
		if allowNegatives {
			if strings.Count(pattern, "!") > 1 ||
				(strings.Count(pattern, "!") == 1 && !strings.HasPrefix(pattern, "!")) {
				err = fmt.Errorf("Object pattern '%s' is not valid. '!' must be first character in a pattern", pattern)
			}
		}
	}
	return err
}

/*
Patterns are very simple, following normal MQ lines except that
they can be prefixed with "!" to exclude them. For example,
  "APP*,DEV*,!SYSTEM*"
I decided not to use a full regexp pattern matcher because it's not really
natural in the MQ world.

Rules for the pattern matching are:
   All positive implies NONE except listed names
   All negative implies ALL except listed names
   Mixed positive and negative entries is done in two phases:
     Remove the negative patterns
     Filter the remaining set with the positive patterns
   Allows patterns like "S*,!SYSTEM*" to still return S.1 but not SYSTEM.DEF.Q

A pattern like "!DEV*,DEV.QUEUE.1" has the negative element
given priority over the positive. So DEV.QUEUE.1 does not match here.
I could reverse the logic in the mixed model, but I think this is preferable.
*/
func FilterRegExp(patterns string, possibleList []string) []string {
	var excludeList []string
	var includeList []string
	var qList []string
	var include bool

	assumeAll := false
	mixed := false
	excludeListString := "" // Comma-separated strings rebuilt for recursive input to this function
	includeListString := ""

	objectPatterns := strings.Split(strings.TrimSpace(patterns), ",")
	for i := 0; i < len(objectPatterns); i++ {
		r := strings.TrimSpace(objectPatterns[i])
		if len(r) == 0 {
			continue
		}
		if strings.HasPrefix(r, "!") {
			// Build a list of patterns to exlude, removing the '!' prefix
			excludeList = append(excludeList, r[1:])
			if excludeListString == "" {
				excludeListString = r
			} else {
				excludeListString = excludeListString + "," + r

			}
		} else {
			includeList = append(includeList, r)
			if includeListString == "" {
				includeListString = r
			} else {
				includeListString = includeListString + "," + r
			}
		}
	}

	//fmt.Printf("  Include list: %v (%d)\n", includeList, len(includeList))
	//fmt.Printf("  Exclude list: %v (%d)\n", excludeList, len(excludeList))

	if len(includeList) > 0 && len(excludeList) == 0 { // all positive
		assumeAll = false
	} else if len(excludeList) > 0 && len(includeList) == 0 { // all negative
		assumeAll = true
	} else {
		assumeAll = true
		mixed = true // Will run a second filter pass
	}

	for j := 0; j < len(possibleList); j++ {
		s := strings.TrimSpace(possibleList[j])
		if len(s) == 0 {
			continue
		}

		if assumeAll {
			include = true
			for i := 0; i < len(excludeList); i++ {
				r := excludeList[i]
				if patternMatch(s, r) {
					include = false
				}
			}
		} else {
			include = false
			for i := 0; i < len(includeList); i++ {
				r := includeList[i]
				if patternMatch(s, r) {
					include = true
				}
			}
		}

		if include {
			qList = append(qList, s)
		} else {
			//fmt.Printf("Excluding %s\n", s)
		}
	}

	if mixed {
		//fmt.Printf("Calling again with patterns %s for %v\n", includeListString, qList)
		qList = FilterRegExp(includeListString, qList)
	}

	//fmt.Printf("Discovered qList = %v\n",qList)
	return qList
}

func patternMatch(s string, r string) bool {
	rc := false
	if strings.HasSuffix(r, "*") {
		if strings.HasPrefix(s, r[:len(r)-1]) {
			rc = true
		}
	} else if s == r {
		rc = true
	}

	//	fmt.Printf("Comparing %s with %s %v\n",s,r,rc)
	return rc
}
