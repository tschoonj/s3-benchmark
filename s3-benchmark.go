// s3-benchmark.go
// Copyright (c) 2017 Wasabi Technology, Inc.

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"code.cloudfoundry.org/bytefmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/aws/awserr"
)

// Global variables
var access_key, secret_key, url_host, bucket, region, sizeArg, multipartThresholdArg, file_type string
var use_multipart_upload bool
var duration_secs, threads, loops int
var object_size uint64
var multipart_threshold uint64
var object_data []byte
var object_data_md5 string
var running_threads, upload_count, download_count, delete_count, upload_slowdown_count, download_slowdown_count, delete_slowdown_count int32
var endtime, upload_finish, download_finish, delete_finish time.Time

const (
	maxRetries = 3
)

func logit(msg string) {
	fmt.Println(msg)
	logfile, _ := os.OpenFile("benchmark.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if logfile != nil {
		logfile.WriteString(time.Now().Format(http.TimeFormat) + ": " + msg + "\n")
		logfile.Close()
	}
}

// Our HTTP transport used for the roundtripper below
var HTTPTransport http.RoundTripper = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	Dial: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).Dial,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 0,
	// Allow an unlimited number of idle connections
	MaxIdleConnsPerHost: 4096,
	MaxIdleConns:        0,
	// But limit their idle time
	IdleConnTimeout: time.Minute,
	// Ignore TLS errors
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}

var httpClient = &http.Client{Transport: HTTPTransport}

func getS3Client() *s3.S3 {
	// Build our config
	creds := credentials.NewStaticCredentials(access_key, secret_key, "")
	loglevel := aws.LogOff
	// Build the rest of the configuration
	awsConfig := &aws.Config{
		Region:               aws.String(region),
		Endpoint:             aws.String(url_host),
		Credentials:          creds,
		LogLevel:             &loglevel,
		S3ForcePathStyle:     aws.Bool(true),
		S3Disable100Continue: aws.Bool(true),
		// Comment following to use default transport
		HTTPClient: &http.Client{Transport: HTTPTransport},
	}
	session := session.New(awsConfig)
	client := s3.New(session)
	if client == nil {
		log.Fatalf("FATAL: Unable to create new client.")
	}
	// Return success
	return client
}

func createBucket(ignore_errors bool) {
	// Get a client
	// client := getS3Client()
	// Create our bucket (may already exist without error)
	svc := s3.New(session.New(), cfg)
	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if _, err := svc.CreateBucket(in); err != nil {
		if strings.Contains(err.Error(), s3.ErrCodeBucketAlreadyOwnedByYou) ||
			strings.Contains(err.Error(), "BucketAlreadyExists") {
			return
		}
		if ignore_errors {
			log.Printf("WARNING: createBucket %s error, ignoring %v", bucket, err)
		} else {
			log.Fatalf("FATAL: Unable to create bucket %s (is your access and secret correct?): %v", bucket, err)
		}
	}
}

func deleteAllObjects() {
	// Get a client
	// client := getS3Client()
	svc := s3.New(session.New(), cfg)
	// in := &s3.DeleteBucketInput{Bucket: aws.String(bucket)}
	// if _, err := svc.DeleteBucket(in); err != nil {
	// 	log.Printf("FATAL: Unable to delete bucket %s : %v", bucket, err)
	// }
	out, err := svc.ListObjects(&s3.ListObjectsInput{Bucket: &bucket})
	if err != nil {
		log.Fatal("can't list objects")
	}
	n := len(out.Contents)
	if n == 0 {
		return
	}
	fmt.Printf("got existing %v objects, try to delete now...\n", n)

	for _, v := range out.Contents {
		svc.DeleteObject(&s3.DeleteObjectInput{
			Bucket: &bucket,
			Key:    v.Key,
		})
	}
	out, err = svc.ListObjects(&s3.ListObjectsInput{Bucket: &bucket})
	if err != nil {
		log.Fatal("can't list objects")
	}
	fmt.Printf("after delete, got %v objects\n", len(out.Contents))

	// // Use multiple routines to do the actual delete
	// var doneDeletes sync.WaitGroup
	// // Loop deleting our versions reading as big a list as we can
	// var keyMarker, versionId *string
	// var err error
	// for loop := 1; ; loop++ {
	// 	// Delete all the existing objects and versions in the bucket
	// 	in := &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), KeyMarker: keyMarker, VersionIdMarker: versionId, MaxKeys: aws.Int64(1000)}
	// 	if listVersions, listErr := client.ListObjectVersions(in); listErr == nil {
	// 		delete := &s3.Delete{Quiet: aws.Bool(true)}
	// 		for _, version := range listVersions.Versions {
	// 			delete.Objects = append(delete.Objects, &s3.ObjectIdentifier{Key: version.Key, VersionId: version.VersionId})
	// 		}
	// 		for _, marker := range listVersions.DeleteMarkers {
	// 			delete.Objects = append(delete.Objects, &s3.ObjectIdentifier{Key: marker.Key, VersionId: marker.VersionId})
	// 		}
	// 		if len(delete.Objects) > 0 {
	// 			// Start a delete routine
	// 			doDelete := func(bucket string, delete *s3.Delete) {
	// 				if _, e := client.DeleteObjects(&s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: delete}); e != nil {
	// 					err = fmt.Errorf("DeleteObjects unexpected failure: %s", e.Error())
	// 				}
	// 				doneDeletes.Done()
	// 			}
	// 			doneDeletes.Add(1)
	// 			go doDelete(bucket, delete)
	// 		}
	// 		// Advance to next versions
	// 		if listVersions.IsTruncated == nil || !*listVersions.IsTruncated {
	// 			break
	// 		}
	// 		keyMarker = listVersions.NextKeyMarker
	// 		versionId = listVersions.NextVersionIdMarker
	// 	} else {
	// 		// The bucket may not exist, just ignore in that case
	// 		if strings.HasPrefix(listErr.Error(), "NoSuchBucket") {
	// 			return
	// 		}
	// 		err = fmt.Errorf("ListObjectVersions unexpected failure: %v", listErr)
	// 		break
	// 	}
	// }
	// // Wait for deletes to finish
	// doneDeletes.Wait()

	// If error, it is fatal
	// if err != nil {
	// 	log.Fatalf("FATAL: Unable to delete objects from bucket: %v", err)
	// }
}

// canonicalAmzHeaders -- return the x-amz headers canonicalized
func canonicalAmzHeaders(req *http.Request) string {
	// Parse out all x-amz headers
	var headers []string
	for header := range req.Header {
		norm := strings.ToLower(strings.TrimSpace(header))
		if strings.HasPrefix(norm, "x-amz") {
			headers = append(headers, norm)
		}
	}
	// Put them in sorted order
	sort.Strings(headers)
	// Now add back the values
	for n, header := range headers {
		headers[n] = header + ":" + strings.Replace(req.Header.Get(header), "\n", " ", -1)
	}
	// Finally, put them back together
	if len(headers) > 0 {
		return strings.Join(headers, "\n") + "\n"
	} else {
		return ""
	}
}

func hmacSHA1(key []byte, content string) []byte {
	mac := hmac.New(sha1.New, key)
	mac.Write([]byte(content))
	return mac.Sum(nil)
}

func setSignature(req *http.Request) {
	// Setup default parameters
	dateHdr := time.Now().UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", dateHdr)
	// Get the canonical resource and header
	canonicalResource := req.URL.EscapedPath()
	canonicalHeaders := canonicalAmzHeaders(req)
	stringToSign := req.Method + "\n" + req.Header.Get("Content-MD5") + "\n" + req.Header.Get("Content-Type") + "\n\n" +
		canonicalHeaders + canonicalResource
	hash := hmacSHA1([]byte(secret_key), stringToSign)
	signature := base64.StdEncoding.EncodeToString(hash)
	req.Header.Set("Authorization", fmt.Sprintf("AWS %s:%s", access_key, signature))
}

func runUploadSinglePart(svc *s3.S3, thread_num int, keys *sync.Map) {
	objnum := atomic.AddInt32(&upload_count, 1)
	fileobj := bytes.NewReader(object_data)

	key := fmt.Sprintf("Object-%d", objnum)
	r := &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   fileobj,
	}

	req, _ := svc.PutObjectRequest(r)
	// Disable payload checksum calculation (very expensive)
	req.HTTPRequest.Header.Add("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	err := req.Send()
	if err != nil {
		atomic.AddInt32(&upload_slowdown_count, 1)
		atomic.AddInt32(&upload_count, -1)
		fmt.Println("upload err", err)
		return
	}
	keys.Store(key, nil)
	fmt.Fprintf(os.Stderr, "upload thread %v, %v\r", thread_num, key)
}

func uploadPart(svc *s3.S3, resp *s3.CreateMultipartUploadOutput, fileBytes []byte, partNumber int) (*s3.CompletedPart, error) {
	tryNum := 1
	partInput := &s3.UploadPartInput{
		Body:          bytes.NewReader(fileBytes),
		Bucket:        resp.Bucket,
		Key:           resp.Key,
		PartNumber:    aws.Int64(int64(partNumber)),
		UploadId:      resp.UploadId,
		ContentLength: aws.Int64(int64(len(fileBytes))),
	}

	for tryNum <= maxRetries {
		uploadResult, err := svc.UploadPart(partInput)
		if err != nil {
			if tryNum == maxRetries {
				if aerr, ok := err.(awserr.Error); ok {
					return nil, aerr
				}
				return nil, err
			}
			tryNum++
		} else {
			return &s3.CompletedPart{
				ETag:       uploadResult.ETag,
				PartNumber: aws.Int64(int64(partNumber)),
			}, nil
		}
	}
	return nil, nil
}

func abortMultipartUpload(svc *s3.S3, resp *s3.CreateMultipartUploadOutput) error {
	fmt.Println("Aborting multipart upload for UploadId#" + *resp.UploadId)
	abortInput := &s3.AbortMultipartUploadInput{
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		UploadId: resp.UploadId,
	}
	_, err := svc.AbortMultipartUpload(abortInput)
	return err
}

func runUploadMultiPart(svc *s3.S3, thread_num int, keys *sync.Map) {
	objnum := atomic.AddInt32(&upload_count, 1)

	key := fmt.Sprintf("Object-%d", objnum)

	input := &s3.CreateMultipartUploadInput{
		Bucket: &bucket,
		Key: &key,
		ContentType: aws.String(file_type),
	}

	resp, err := svc.CreateMultipartUpload(input)
	if err != nil {
		atomic.AddInt32(&upload_slowdown_count, 1)
		atomic.AddInt32(&upload_count, -1)
		fmt.Println("upload err", err)
		return
	}

	var curr, partLength uint64
	var remaining = object_size
	var completedParts []*s3.CompletedPart
	partNumber := 1
	for curr = 0; remaining != 0; curr += partLength {
		if remaining < multipart_threshold {
			partLength = remaining
		} else {
			partLength = multipart_threshold
		}
		completedPart, err := uploadPart(svc, resp, object_data[curr:curr+partLength], partNumber)
		if err != nil {
			atomic.AddInt32(&upload_slowdown_count, 1)
			atomic.AddInt32(&upload_count, -1)
			fmt.Println("upload err", err)
			err := abortMultipartUpload(svc, resp)
			if err != nil {
				fmt.Println(err.Error())
			}
			return
		}
		remaining -= partLength
		partNumber++
		completedParts = append(completedParts, completedPart)
	}

	_, err = completeMultipartUpload(svc, resp, completedParts)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	keys.Store(key, nil)
	fmt.Fprintf(os.Stderr, "upload thread %v, %v\r", thread_num, key)
}

func completeMultipartUpload(svc *s3.S3, resp *s3.CreateMultipartUploadOutput, completedParts []*s3.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
	completeInput := &s3.CompleteMultipartUploadInput{
		Bucket:   resp.Bucket,
		Key:      resp.Key,
		UploadId: resp.UploadId,
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: completedParts,
		},
	}
	return svc.CompleteMultipartUpload(completeInput)
}

func runUpload(thread_num int, keys *sync.Map) {
	svc := s3.New(session.New(), cfg)
	for time.Now().Before(endtime) {
		if use_multipart_upload {
			runUploadMultiPart(svc, thread_num, keys)
		} else {
			runUploadSinglePart(svc, thread_num, keys)
		}
	}
	// Remember last done time
	upload_finish = time.Now()
	// One less thread
	atomic.AddInt32(&running_threads, -1)
}

func runDownload(thread_num int, keys *sync.Map) {
	errcnt := 0
	svc := s3.New(session.New(), cfg)

	keys.Range(func(k, value interface{}) bool {

		// 	objnum := atomic.AddInt32(&delete_count, 1)
		// 	if objnum > upload_count {
		// 		delete_count = 0
		// 	}
		// 	key := fmt.Sprintf("Object-%d", objnum)

		if time.Now().After(endtime) {
			// fmt.Println("time ended for download")
			return false
		}
		var key string
		var ok bool
		if key, ok = k.(string); !ok {
			log.Fatal("convert key back error")
		}

		fmt.Fprintf(os.Stderr, "download thread %v, %v\r", thread_num, key)

		r := &s3.GetObjectInput{
			Bucket: &bucket,
			Key:    &key,
		}

		req, resp := svc.GetObjectRequest(r)
		err := req.Send()
		if err != nil {
			errcnt++
			atomic.AddInt32(&download_slowdown_count, 1)
			atomic.AddInt32(&download_count, -1)
			fmt.Println("download err", err)
			//break
		}
		if err == nil {
			_, err = io.Copy(ioutil.Discard, resp.Body)
		}
		if errcnt > 2 {
			return false
		}
		atomic.AddInt32(&download_count, 1)

		// prefix := fmt.Sprintf("%s/%s/Object-%d", url_host, bucket, objnum)
		// req, _ := http.NewRequest("GET", prefix, nil)
		// setSignature(req)
		// if resp, err := httpClient.Do(req); err != nil {
		// 	log.Fatalf("FATAL: Error downloading object %s: %v", prefix, err)
		// } else if resp != nil && resp.Body != nil {
		// 	if resp.StatusCode == http.StatusServiceUnavailable {
		// 		atomic.AddInt32(&download_slowdown_count, 1)
		// 		atomic.AddInt32(&download_count, -1)
		// 	} else {
		// 		io.Copy(ioutil.Discard, resp.Body)
		// 	}
		// }

		return true
	})

	// Remember last done time
	download_finish = time.Now()
	// One less thread
	atomic.AddInt32(&running_threads, -1)
}

func runDelete(thread_num int) {
	errcnt := 0
	svc := s3.New(session.New(), cfg)
	for {
		objnum := atomic.AddInt32(&delete_count, 1)
		if objnum > upload_count {
			break
		}
		key := fmt.Sprintf("Object-%d", objnum)
		r := &s3.DeleteObjectInput{
			Bucket: &bucket,
			Key:    &key,
		}

		req, out := svc.DeleteObjectRequest(r)
		err := req.Send()
		if err != nil {
			errcnt++
			atomic.AddInt32(&delete_slowdown_count, 1)
			atomic.AddInt32(&delete_count, -1)
			fmt.Println("download err", err, "out", out.String())
			//break
		}
		if errcnt > 2 {
			break
		}
		fmt.Fprintf(os.Stderr, "delete thread %v, %v\r", thread_num, key)

		// prefix := fmt.Sprintf("%s/%s/Object-%d", url_host, bucket, objnum)
		// req, _ := http.NewRequest("DELETE", prefix, nil)
		// setSignature(req)
		// if resp, err := httpClient.Do(req); err != nil {
		// 	log.Fatalf("FATAL: Error deleting object %s: %v", prefix, err)
		// } else if resp != nil && resp.StatusCode == http.StatusServiceUnavailable {
		// 	atomic.AddInt32(&delete_slowdown_count, 1)
		// 	atomic.AddInt32(&delete_count, -1)
		// }
	}
	// Remember last done time
	delete_finish = time.Now()
	// One less thread
	atomic.AddInt32(&running_threads, -1)
}

var cfg *aws.Config

func init() {
	// Parse command line
	myflag := flag.NewFlagSet("myflag", flag.ExitOnError)
	myflag.StringVar(&access_key, "a", os.Getenv("AWS_ACCESS_KEY_ID"), "Access key")
	myflag.StringVar(&secret_key, "s", os.Getenv("AWS_SECRET_ACCESS_KEY"), "Secret key")
	myflag.StringVar(&url_host, "u", os.Getenv("AWS_HOST"), "URL for host with method prefix")
	myflag.StringVar(&bucket, "b", "loadgen", "Bucket for testing")
	myflag.StringVar(&region, "r", "us-east-1", "Region for testing")
	myflag.IntVar(&duration_secs, "d", 60, "Duration of each test in seconds")
	myflag.IntVar(&threads, "t", 1, "Number of threads to run")
	myflag.IntVar(&loops, "l", 1, "Number of times to repeat test")
	myflag.StringVar(&sizeArg, "z", "1M", "Size of objects in bytes with postfix K, M, and G")
	myflag.StringVar(&multipartThresholdArg, "m", "5G", "Multipart upload threshold")
	if err := myflag.Parse(os.Args[1:]); err != nil {
		os.Exit(1)
	}

	// Check the arguments
	if access_key == "" {
		log.Fatal("Missing argument -a for access key.")
	}
	if secret_key == "" {
		log.Fatal("Missing argument -s for secret key.")
	}
	if url_host == "" {
		log.Fatal("Missing argument -s for host endpoint.")
	}
	var err error
	if object_size, err = bytefmt.ToBytes(sizeArg); err != nil {
		log.Fatalf("Invalid -z argument for object size: %v", err)
	}
	if multipart_threshold, err = bytefmt.ToBytes(multipartThresholdArg) ; err != nil {
		log.Fatalf("Invalid -m argument for multipart threshold: %v", err)
	}
	if multipart_threshold > 5 * bytefmt.GIGABYTE {
		log.Fatal("The multipart threshold cannot be greater than 5GB")
	}
	use_multipart_upload = object_size > multipart_threshold
}

func main() {
	// Hello
	fmt.Println("Wasabi benchmark program v2.0")

	//fmt.Println("accesskey:", access_key, "secretkey:", secret_key)
	cfg = &aws.Config{
		Endpoint:    aws.String(url_host),
		Credentials: credentials.NewStaticCredentials(access_key, secret_key, ""),
		Region:      aws.String(region),
		// DisableParamValidation:  aws.Bool(true),
		DisableComputeChecksums: aws.Bool(true),
		S3ForcePathStyle:        aws.Bool(true),
	}

	// Echo the parameters
	logit(fmt.Sprintf("Parameters: url=%s, bucket=%s, region=%s, duration=%d, threads=%d, loops=%d, size=%s, multipart-threshold=%s, use-multipart-upload=%t",
		url_host, bucket, region, duration_secs, threads, loops, sizeArg, multipartThresholdArg, use_multipart_upload))

	// Initialize data for the bucket
	object_data = make([]byte, object_size)
	file_type = http.DetectContentType(object_data)
	rand.Read(object_data)
	hasher := md5.New()
	hasher.Write(object_data)
	object_data_md5 = base64.StdEncoding.EncodeToString(hasher.Sum(nil))

	// Create the bucket and delete all the objects
	createBucket(true)
	deleteAllObjects()

	var uploadspeed, downloadspeed float64

	// Loop running the tests
	for loop := 1; loop <= loops; loop++ {

		// reset counters
		upload_count = 0
		upload_slowdown_count = 0
		download_count = 0
		download_slowdown_count = 0
		delete_count = 0
		delete_slowdown_count = 0

		keys := &sync.Map{}

		// Run the upload case
		running_threads = int32(threads)
		starttime := time.Now()
		endtime = starttime.Add(time.Second * time.Duration(duration_secs))
		for n := 1; n <= threads; n++ {
			go runUpload(n, keys)
		}

		// Wait for it to finish
		for atomic.LoadInt32(&running_threads) > 0 {
			time.Sleep(time.Millisecond)
		}
		upload_time := upload_finish.Sub(starttime).Seconds()

		bps := float64(uint64(upload_count)*object_size) / upload_time
		logit(fmt.Sprintf("Loop %d: PUT time %.1f secs, objects = %d, speed = %sB/sec, %.1f operations/sec. Slowdowns = %d",
			loop, upload_time, upload_count, bytefmt.ByteSize(uint64(bps)), float64(upload_count)/upload_time, upload_slowdown_count))

		uploadspeed = bps / bytefmt.MEGABYTE
		// count := 0
		// keys.Range(func(k, value interface{}) bool {
		// 	count++
		// 	return true
		// })
		// fmt.Println("map got ", count)

		// Run the download case
		running_threads = int32(threads)
		starttime = time.Now()
		endtime = starttime.Add(time.Second * time.Duration(duration_secs))
		for n := 1; n <= threads; n++ {
			go runDownload(n, keys)
		}

		// Wait for it to finish
		for atomic.LoadInt32(&running_threads) > 0 {
			time.Sleep(time.Millisecond)
		}
		download_time := download_finish.Sub(starttime).Seconds()

		bps = float64(uint64(download_count)*object_size) / download_time
		logit(fmt.Sprintf("Loop %d: GET time %.1f secs, objects = %d, speed = %sB/sec, %.1f operations/sec. Slowdowns = %d",
			loop, download_time, download_count, bytefmt.ByteSize(uint64(bps)), float64(download_count)/download_time, download_slowdown_count))

		downloadspeed = bps / bytefmt.MEGABYTE

		// Run the delete case
		running_threads = int32(threads)
		starttime = time.Now()
		endtime = starttime.Add(time.Second * time.Duration(duration_secs))
		for n := 1; n <= threads; n++ {
			go runDelete(n)
		}

		// Wait for it to finish
		for atomic.LoadInt32(&running_threads) > 0 {
			time.Sleep(time.Millisecond)
		}
		delete_time := delete_finish.Sub(starttime).Seconds()

		logit(fmt.Sprintf("Loop %d: DELETE time %.1f secs, %.1f deletes/sec. Slowdowns = %d",
			loop, delete_time, float64(upload_count)/delete_time, delete_slowdown_count))
	}

	// All done
	name := strings.Split(strings.TrimPrefix(url_host, "http://"), ".")[0]
	fmt.Printf("result title: name-concurrency-size, uloadspeed, downloadspeed\n")
	fmt.Printf("result csv: %v-%v-%v,%.2f,%.2f\n", name, threads, sizeArg, uploadspeed, downloadspeed)
}
