package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Implement a dummy flush writer.
type flushWriter struct {
	io.Writer
}

// Flush writer is a dummy writer compatible with http.Flusher and http.ResponseWriter.
func (f *flushWriter) Flush()                            {}
func (f *flushWriter) Write(b []byte) (n int, err error) { return f.Writer.Write(b) }
func (f *flushWriter) Header() http.Header               { return http.Header{} }
func (f *flushWriter) WriteHeader(code int)              {}

func newFlushWriter(writer io.Writer) http.ResponseWriter {
	return &flushWriter{writer}
}

// Tests write notification code.
func TestWriteNotification(t *testing.T) {
	// Initialize a new test config.
	root, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Unable to initialize test config %s", err)
	}
	defer removeAll(root)

	var buffer bytes.Buffer
	// Collection of test cases for each event writer.
	testCases := []struct {
		writer http.ResponseWriter
		event  map[string][]NotificationEvent
		err    error
	}{
		// Invalid input argument with writer `nil` - Test - 1
		{
			writer: nil,
			event:  nil,
			err:    errInvalidArgument,
		},
		// Invalid input argument with event `nil` - Test - 2
		{
			writer: newFlushWriter(ioutil.Discard),
			event:  nil,
			err:    errInvalidArgument,
		},
		// Unmarshal and write, validate last 5 bytes. - Test - 3
		{
			writer: newFlushWriter(&buffer),
			event: map[string][]NotificationEvent{
				"Records": {newNotificationEvent(eventData{
					Type:   ObjectCreatedPut,
					Bucket: "testbucket",
					ObjInfo: ObjectInfo{
						Name: "key",
					},
					ReqParams: map[string]string{
						"ip": "10.1.10.1",
					}}),
				},
			},
			err: nil,
		},
	}
	// Validates all the testcases for writing notification.
	for _, testCase := range testCases {
		err := writeNotification(testCase.writer, testCase.event)
		if err != testCase.err {
			t.Errorf("Unable to write notification %s", err)
		}
		// Validates if the ending string has 'crlf'
		if err == nil && !bytes.HasSuffix(buffer.Bytes(), crlf) {
			buf := buffer.Bytes()[buffer.Len()-5 : 0]
			t.Errorf("Invalid suffix found from the writer last 5 bytes %s, expected `\r\n`", string(buf))
		}
		// Not printing 'buf' on purpose, validates look for string '10.1.10.1'.
		if err == nil && !bytes.Contains(buffer.Bytes(), []byte("10.1.10.1")) {
			// Enable when debugging)
			// fmt.Println(string(buffer.Bytes()))
			t.Errorf("Requested content couldn't be found, expected `10.1.10.1`")
		}
	}
}

func TestSendBucketNotification(t *testing.T) {
	// Initialize a new test config.
	root, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Unable to initialize test config %s", err)
	}
	defer removeAll(root)

	eventCh := make(chan []NotificationEvent)

	// Create a Pipe with FlushWriter on the write-side and bufio.Scanner
	// on the reader-side to receive notification over the listen channel in a
	// synchronized manner.
	pr, pw := io.Pipe()
	fw := newFlushWriter(pw)
	scanner := bufio.NewScanner(pr)
	// Start a go-routine to wait for notification events.
	go func(listenerCh <-chan []NotificationEvent) {
		sendBucketNotification(fw, listenerCh)
	}(eventCh)

	// Construct notification events to be passed on the events channel.
	var events []NotificationEvent
	evTypes := []EventName{
		ObjectCreatedPut,
		ObjectCreatedPost,
		ObjectCreatedCopy,
		ObjectCreatedCompleteMultipartUpload,
	}
	for _, evType := range evTypes {
		events = append(events, newNotificationEvent(eventData{
			Type: evType,
		}))
	}
	// Send notification events to the channel on which sendBucketNotification
	// is waiting on.
	eventCh <- events

	// Read from the pipe connected to the ResponseWriter.
	scanner.Scan()
	notificationBytes := scanner.Bytes()

	// Close the read-end and send an empty notification event on the channel
	// to signal sendBucketNotification to terminate.
	pr.Close()
	eventCh <- []NotificationEvent{}
	close(eventCh)

	// Checking if the notification are the same as those sent over the channel.
	var notifications map[string][]NotificationEvent
	err = json.Unmarshal(notificationBytes, &notifications)
	if err != nil {
		t.Fatal("Failed to Unmarshal notification")
	}
	records := notifications["Records"]
	for i, rec := range records {
		if rec.EventName == evTypes[i].String() {
			continue
		}
		t.Errorf("Failed to receive %d event %s", i, evTypes[i].String())
	}
}

func testGetBucketNotificationHandler(obj ObjectLayer, instanceType string, t TestErrHandler) {
	// get random bucket name.
	randBucket := getRandomBucketName()
	noNotificationBucket := "nonotification"
	invalidBucket := "Invalid\\Bucket"

	// Create buckets for the following test cases.
	for _, bucket := range []string{randBucket, noNotificationBucket} {
		err := obj.MakeBucket(bucket)
		if err != nil {
			// failed to create newbucket, abort.
			t.Fatalf("Failed to create bucket %s %s : %s", bucket,
				instanceType, err)
		}
	}

	// Initialize sample bucket notification config.
	sampleNotificationBytes := []byte("<NotificationConfiguration><TopicConfiguration>" +
		"<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Filter>" +
		"<S3Key></S3Key></Filter><Id></Id><Topic>arn:minio:sns:us-east-1:1474332374:listen</Topic>" +
		"</TopicConfiguration></NotificationConfiguration>")

	emptyNotificationBytes := []byte("<NotificationConfiguration></NotificationConfiguration>")

	// Register the API end points with XL/FS object layer.
	apiRouter := initTestAPIEndPoints(obj, []string{
		"GetBucketNotification",
		"PutBucketNotification",
	})

	// initialize the server and obtain the credentials and root.
	// credentials are necessary to sign the HTTP request.
	rootPath, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Init Test config failed")
	}
	// remove the root folder after the test ends.
	defer removeAll(rootPath)

	credentials := serverConfig.GetCredential()

	//Initialize global event notifier with mock queue targets.
	err = initEventNotifier(obj)
	if err != nil {
		t.Fatalf("Test %s: Failed to initialize mock event notifier %v",
			instanceType, err)
	}
	// Initialize httptest recorder.
	rec := httptest.NewRecorder()

	// Prepare notification config for one of the test cases.
	req, err := newTestSignedRequestV4("PUT", getPutBucketNotificationURL("", randBucket),
		int64(len(sampleNotificationBytes)), bytes.NewReader(sampleNotificationBytes),
		credentials.AccessKeyID, credentials.SecretAccessKey)
	if err != nil {
		t.Fatalf("Test %d %s: Failed to create HTTP request for PutBucketNotification: <ERROR> %v",
			1, instanceType, err)
	}

	apiRouter.ServeHTTP(rec, req)

	type testKind int
	const (
		CompareBytes testKind = iota
		CheckStatus
		InvalidAuth
	)
	testCases := []struct {
		bucketName                string
		kind                      testKind
		expectedNotificationBytes []byte
		expectedHTTPCode          int
	}{
		{randBucket, CompareBytes, sampleNotificationBytes, http.StatusOK},
		{randBucket, InvalidAuth, nil, http.StatusBadRequest},
		{noNotificationBucket, CompareBytes, emptyNotificationBytes, http.StatusOK},
		{invalidBucket, CheckStatus, nil, http.StatusBadRequest},
	}
	signatureMismatchCode := getAPIError(ErrContentSHA256Mismatch).Code
	for i, test := range testCases {
		testRec := httptest.NewRecorder()
		testReq, tErr := newTestSignedRequestV4("GET", getGetBucketNotificationURL("", test.bucketName),
			int64(0), nil, credentials.AccessKeyID, credentials.SecretAccessKey)
		if tErr != nil {
			t.Fatalf("Test %d: %s: Failed to create HTTP testRequest for GetBucketNotification: <ERROR> %v",
				i+1, instanceType, tErr)
		}

		// Set X-Amz-Content-SHA256 in header different from what was used to calculate Signature.
		if test.kind == InvalidAuth {
			// Triggering a authentication type check failure.
			testReq.Header.Set("x-amz-content-sha256", "somethingElse")
		}

		apiRouter.ServeHTTP(testRec, testReq)

		switch test.kind {
		case CompareBytes:
			rspBytes, rErr := ioutil.ReadAll(testRec.Body)
			if rErr != nil {
				t.Errorf("Test %d: %s: Failed to read response body: <ERROR> %v", i+1, instanceType, rErr)
			}
			if !bytes.Equal(rspBytes, test.expectedNotificationBytes) {
				t.Errorf("Test %d: %s: Notification config doesn't match expected value %s: <ERROR> %v",
					i+1, instanceType, string(test.expectedNotificationBytes), err)
			}
		case InvalidAuth:
			rspBytes, rErr := ioutil.ReadAll(testRec.Body)
			if rErr != nil {
				t.Errorf("Test %d: %s: Failed to read response body: <ERROR> %v", i+1, instanceType, rErr)
			}
			var errCode APIError
			xErr := xml.Unmarshal(rspBytes, &errCode)
			if xErr != nil {
				t.Errorf("Test %d: %s: Failed to unmarshal error XML: <ERROR> %v", i+1, instanceType, xErr)

			}

			if errCode.Code != signatureMismatchCode {
				t.Errorf("Test %d: %s: Expected error code %s but received %s: <ERROR> %v", i+1,
					instanceType, signatureMismatchCode, errCode.Code, err)

			}
			fallthrough
		case CheckStatus:
			if testRec.Code != test.expectedHTTPCode {
				t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
					i+1, instanceType, test.expectedHTTPCode, testRec.Code, err)
			}
		}
	}

	// Nil Object layer
	nilAPIRouter := initTestAPIEndPoints(nil, []string{
		"GetBucketNotification",
		"PutBucketNotification",
	})
	testRec := httptest.NewRecorder()
	testReq, tErr := newTestSignedRequestV4("GET", getGetBucketNotificationURL("", randBucket),
		int64(0), nil, credentials.AccessKeyID, credentials.SecretAccessKey)
	if tErr != nil {
		t.Fatalf("Test %d: %s: Failed to create HTTP testRequest for GetBucketNotification: <ERROR> %v",
			len(testCases)+1, instanceType, tErr)
	}
	nilAPIRouter.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusServiceUnavailable {
		t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
			len(testCases)+1, instanceType, http.StatusServiceUnavailable, testRec.Code, err)
	}
}

func TestGetBucketNotificationHandler(t *testing.T) {
	ExecObjectLayerTest(t, testGetBucketNotificationHandler)
}

func testPutBucketNotificationHandler(obj ObjectLayer, instanceType string, t TestErrHandler) {
	invalidBucket := "Invalid\\Bucket"
	// get random bucket name.
	randBucket := getRandomBucketName()

	err := obj.MakeBucket(randBucket)
	if err != nil {
		// failed to create randBucket, abort.
		t.Fatalf("Failed to create bucket %s %s : %s", randBucket,
			instanceType, err)
	}

	sampleNotificationBytes := []byte("<NotificationConfiguration><TopicConfiguration>" +
		"<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Filter>" +
		"<S3Key></S3Key></Filter><Id></Id><Topic>arn:minio:sns:us-east-1:1474332374:listen</Topic>" +
		"</TopicConfiguration></NotificationConfiguration>")

	// Register the API end points with XL/FS object layer.
	apiRouter := initTestAPIEndPoints(obj, []string{
		"GetBucketNotification",
		"PutBucketNotification",
	})

	// initialize the server and obtain the credentials and root.
	// credentials are necessary to sign the HTTP request.
	rootPath, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Init Test config failed")
	}
	// remove the root folder after the test ends.
	defer removeAll(rootPath)

	credentials := serverConfig.GetCredential()

	//Initialize global event notifier with mock queue targets.
	err = initEventNotifier(obj)
	if err != nil {
		t.Fatalf("Test %s: Failed to initialize mock event notifier %v",
			instanceType, err)
	}

	signatureMismatchError := getAPIError(ErrContentSHA256Mismatch)
	missingContentLengthError := getAPIError(ErrMissingContentLength)
	type testKind int
	const (
		CompareBytes testKind = iota
		CheckStatus
		InvalidAuth
		MissingContentLength
		ChunkedEncoding
	)
	testCases := []struct {
		bucketName                string
		kind                      testKind
		expectedNotificationBytes []byte
		expectedHTTPCode          int
		expectedAPIError          string
	}{
		{randBucket, CompareBytes, sampleNotificationBytes, http.StatusOK, ""},
		{randBucket, ChunkedEncoding, sampleNotificationBytes, http.StatusOK, ""},
		{randBucket, InvalidAuth, nil, signatureMismatchError.HTTPStatusCode, signatureMismatchError.Code},
		{randBucket, MissingContentLength, nil, missingContentLengthError.HTTPStatusCode, missingContentLengthError.Code},
		{invalidBucket, CheckStatus, nil, http.StatusBadRequest, ""},
	}
	for i, test := range testCases {
		testRec := httptest.NewRecorder()
		testReq, tErr := newTestSignedRequestV4("PUT", getPutBucketNotificationURL("", test.bucketName),
			int64(len(test.expectedNotificationBytes)), bytes.NewReader(test.expectedNotificationBytes),
			credentials.AccessKeyID, credentials.SecretAccessKey)
		if tErr != nil {
			t.Fatalf("Test %d: %s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v",
				i+1, instanceType, tErr)
		}

		// Set X-Amz-Content-SHA256 in header different from what was used to calculate Signature.
		switch test.kind {
		case InvalidAuth:
			// Triggering a authentication type check failure.
			testReq.Header.Set("x-amz-content-sha256", "somethingElse")
		case MissingContentLength:
			testReq.ContentLength = -1
		case ChunkedEncoding:
			testReq.ContentLength = -1
			testReq.TransferEncoding = append(testReq.TransferEncoding, "chunked")
		}

		apiRouter.ServeHTTP(testRec, testReq)

		switch test.kind {
		case CompareBytes:

			testReq, tErr = newTestSignedRequestV4("GET", getGetBucketNotificationURL("", test.bucketName),
				int64(0), nil, credentials.AccessKeyID, credentials.SecretAccessKey)
			if tErr != nil {
				t.Fatalf("Test %d: %s: Failed to create HTTP testRequest for GetBucketNotification: <ERROR> %v",
					i+1, instanceType, tErr)
			}
			apiRouter.ServeHTTP(testRec, testReq)

			rspBytes, rErr := ioutil.ReadAll(testRec.Body)
			if rErr != nil {
				t.Errorf("Test %d: %s: Failed to read response body: <ERROR> %v", i+1, instanceType, rErr)
			}
			if !bytes.Equal(rspBytes, test.expectedNotificationBytes) {
				t.Errorf("Test %d: %s: Notification config doesn't match expected value %s: <ERROR> %v",
					i+1, instanceType, string(test.expectedNotificationBytes), err)
			}
		case MissingContentLength, InvalidAuth:
			rspBytes, rErr := ioutil.ReadAll(testRec.Body)
			if rErr != nil {
				t.Errorf("Test %d: %s: Failed to read response body: <ERROR> %v", i+1, instanceType, rErr)
			}
			var errCode APIError
			xErr := xml.Unmarshal(rspBytes, &errCode)
			if xErr != nil {
				t.Errorf("Test %d: %s: Failed to unmarshal error XML: <ERROR> %v", i+1, instanceType, xErr)

			}

			if errCode.Code != test.expectedAPIError {
				t.Errorf("Test %d: %s: Expected error code %s but received %s: <ERROR> %v", i+1,
					instanceType, test.expectedAPIError, errCode.Code, err)

			}
			fallthrough
		case CheckStatus:
			if testRec.Code != test.expectedHTTPCode {
				t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
					i+1, instanceType, test.expectedHTTPCode, testRec.Code, err)
			}
		}
	}

	// Nil Object layer
	nilAPIRouter := initTestAPIEndPoints(nil, []string{
		"GetBucketNotification",
		"PutBucketNotification",
	})
	testRec := httptest.NewRecorder()
	testReq, tErr := newTestSignedRequestV4("PUT", getPutBucketNotificationURL("", randBucket),
		int64(len(sampleNotificationBytes)), bytes.NewReader(sampleNotificationBytes),
		credentials.AccessKeyID, credentials.SecretAccessKey)
	if tErr != nil {
		t.Fatalf("Test %d: %s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v",
			len(testCases)+1, instanceType, tErr)
	}
	nilAPIRouter.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusServiceUnavailable {
		t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
			len(testCases)+1, instanceType, http.StatusServiceUnavailable, testRec.Code, err)
	}
}

func TestPutBucketNotificationHandler(t *testing.T) {
	ExecObjectLayerTest(t, testPutBucketNotificationHandler)
}

func testListenBucketNotificationHandler(obj ObjectLayer, instanceType string, t TestErrHandler) {
	invalidBucket := "Invalid\\Bucket"
	noNotificationBucket := "nonotificationbucket"
	// get random bucket name.
	randBucket := getRandomBucketName()
	for _, bucket := range []string{randBucket, noNotificationBucket} {
		err := obj.MakeBucket(bucket)
		if err != nil {
			// failed to create bucket, abort.
			t.Fatalf("Failed to create bucket %s %s : %s", bucket,
				instanceType, err)
		}
	}

	sampleNotificationBytes := []byte("<NotificationConfiguration><TopicConfiguration>" +
		"<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Filter>" +
		"<S3Key></S3Key></Filter><Id></Id><Topic>arn:minio:sns:us-east-1:1474332374:listen</Topic>" +
		"</TopicConfiguration></NotificationConfiguration>")

	// Register the API end points with XL/FS object layer.
	apiRouter := initTestAPIEndPoints(obj, []string{
		"PutBucketNotification",
		"ListenBucketNotification",
		"PutObject",
	})

	// initialize the server and obtain the credentials and root.
	// credentials are necessary to sign the HTTP request.
	rootPath, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Init Test config failed")
	}
	// remove the root folder after the test ends.
	defer removeAll(rootPath)

	credentials := serverConfig.GetCredential()

	// Initialize global event notifier with mock queue targets.
	err = initEventNotifier(obj)
	if err != nil {
		t.Fatalf("Test %s: Failed to initialize mock event notifier %v",
			instanceType, err)
	}
	testRec := httptest.NewRecorder()
	testReq, tErr := newTestSignedRequestV4("PUT", getPutBucketNotificationURL("", randBucket),
		int64(len(sampleNotificationBytes)), bytes.NewReader(sampleNotificationBytes),
		credentials.AccessKeyID, credentials.SecretAccessKey)
	if tErr != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v", instanceType, tErr)
	}
	apiRouter.ServeHTTP(testRec, testReq)

	signatureMismatchError := getAPIError(ErrContentSHA256Mismatch)
	type testKind int
	const (
		CheckStatus testKind = iota
		InvalidAuth
		AsyncHandler
	)
	tooBigPrefix := string(bytes.Repeat([]byte("a"), 1025))
	validEvents := []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}
	invalidEvents := []string{"invalidEvent"}
	testCases := []struct {
		bucketName       string
		prefix           string
		suffix           string
		events           []string
		kind             testKind
		expectedHTTPCode int
		expectedAPIError string
	}{
		// FIXME: Need to find a way to run valid listen bucket notification test case without blocking the unit test.
		{randBucket, "", "", invalidEvents, CheckStatus, signatureMismatchError.HTTPStatusCode, ""},
		{randBucket, tooBigPrefix, "", validEvents, CheckStatus, http.StatusBadRequest, ""},
		{invalidBucket, "", "", validEvents, CheckStatus, http.StatusBadRequest, ""},
		{randBucket, "", "", validEvents, InvalidAuth, signatureMismatchError.HTTPStatusCode, signatureMismatchError.Code},
	}

	for i, test := range testCases {
		testRec = httptest.NewRecorder()
		testReq, tErr = newTestSignedRequestV4("GET",
			getListenBucketNotificationURL("", test.bucketName, test.prefix, test.suffix, test.events),
			0, nil, credentials.AccessKeyID, credentials.SecretAccessKey)
		if tErr != nil {
			t.Fatalf("%s: Failed to create HTTP testRequest for ListenBucketNotification: <ERROR> %v", instanceType, tErr)
		}
		// Set X-Amz-Content-SHA256 in header different from what was used to calculate Signature.
		if test.kind == InvalidAuth {
			// Triggering a authentication type check failure.
			testReq.Header.Set("x-amz-content-sha256", "somethingElse")
		}
		if test.kind == AsyncHandler {
			go apiRouter.ServeHTTP(testRec, testReq)
		} else {
			apiRouter.ServeHTTP(testRec, testReq)
			switch test.kind {
			case InvalidAuth:
				rspBytes, rErr := ioutil.ReadAll(testRec.Body)
				if rErr != nil {
					t.Errorf("Test %d: %s: Failed to read response body: <ERROR> %v", i+1, instanceType, rErr)
				}
				var errCode APIError
				xErr := xml.Unmarshal(rspBytes, &errCode)
				if xErr != nil {
					t.Errorf("Test %d: %s: Failed to unmarshal error XML: <ERROR> %v", i+1, instanceType, xErr)

				}

				if errCode.Code != test.expectedAPIError {
					t.Errorf("Test %d: %s: Expected error code %s but received %s: <ERROR> %v", i+1,
						instanceType, test.expectedAPIError, errCode.Code, err)

				}
				fallthrough
			case CheckStatus:
				if testRec.Code != test.expectedHTTPCode {
					t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
						i+1, instanceType, test.expectedHTTPCode, testRec.Code, err)
				}
			}
		}
	}

	// Nil Object layer
	nilAPIRouter := initTestAPIEndPoints(nil, []string{
		"PutBucketNotification",
		"ListenBucketNotification",
	})
	testRec = httptest.NewRecorder()
	testReq, tErr = newTestSignedRequestV4("GET",
		getListenBucketNotificationURL("", randBucket, "", "*.jpg", []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"}),
		0, nil, credentials.AccessKeyID, credentials.SecretAccessKey)
	if tErr != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for ListenBucketNotification: <ERROR> %v", instanceType, tErr)
	}
	nilAPIRouter.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusServiceUnavailable {
		t.Errorf("Test %d: %s: expected HTTP code %d, but received %d: <ERROR> %v",
			1, instanceType, http.StatusServiceUnavailable, testRec.Code, err)
	}
}

func TestListenBucketNotificationHandler(t *testing.T) {
	ExecObjectLayerTest(t, testListenBucketNotificationHandler)
}

func testRemoveNotificationConfig(obj ObjectLayer, instanceType string, t TestErrHandler) {
	invalidBucket := "Invalid\\Bucket"
	// get random bucket name.
	randBucket := getRandomBucketName()

	err := obj.MakeBucket(randBucket)
	if err != nil {
		// failed to create bucket, abort.
		t.Fatalf("Failed to create bucket %s %s : %s", randBucket,
			instanceType, err)
	}

	sampleNotificationBytes := []byte("<NotificationConfiguration><TopicConfiguration>" +
		"<Event>s3:ObjectCreated:*</Event><Event>s3:ObjectRemoved:*</Event><Filter>" +
		"<S3Key></S3Key></Filter><Id></Id><Topic>arn:minio:sns:us-east-1:1474332374:listen</Topic>" +
		"</TopicConfiguration></NotificationConfiguration>")

	// Register the API end points with XL/FS object layer.
	apiRouter := initTestAPIEndPoints(obj, []string{
		"PutBucketNotification",
		"ListenBucketNotification",
	})

	// initialize the server and obtain the credentials and root.
	// credentials are necessary to sign the HTTP request.
	rootPath, err := newTestConfig("us-east-1")
	if err != nil {
		t.Fatalf("Init Test config failed")
	}
	// remove the root folder after the test ends.
	defer removeAll(rootPath)

	credentials := serverConfig.GetCredential()

	//Initialize global event notifier with mock queue targets.
	err = initEventNotifier(obj)
	if err != nil {
		t.Fatalf("Test %s: Failed to initialize mock event notifier %v",
			instanceType, err)
	}
	// Set sample bucket notification on randBucket.
	testRec := httptest.NewRecorder()
	testReq, tErr := newTestSignedRequestV4("PUT", getPutBucketNotificationURL("", randBucket),
		int64(len(sampleNotificationBytes)), bytes.NewReader(sampleNotificationBytes),
		credentials.AccessKeyID, credentials.SecretAccessKey)
	if tErr != nil {
		t.Fatalf("%s: Failed to create HTTP testRequest for PutBucketNotification: <ERROR> %v", instanceType, tErr)
	}
	apiRouter.ServeHTTP(testRec, testReq)

	testCases := []struct {
		bucketName  string
		expectedErr error
	}{
		{invalidBucket, BucketNameInvalid{Bucket: invalidBucket}},
		{randBucket, nil},
	}
	for i, test := range testCases {
		tErr := removeNotificationConfig(test.bucketName, obj)
		if tErr != test.expectedErr {
			t.Errorf("Test %d: %s expected error %v, but received %v", i+1, instanceType, test.expectedErr, tErr)
		}
	}
}

func TestRemoveNotificationConfig(t *testing.T) {
	ExecObjectLayerTest(t, testRemoveNotificationConfig)
}
