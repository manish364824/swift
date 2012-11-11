// This package provides an easy to use interface to Swift / Openstack
// Object Storage / Rackspace cloud files from the Go Language
package swift

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultUserAgent = "goswift/1.0"         // Default user agent
	DefaultRetries   = 3                     // Default number of retries on token expiry
	TimeFormat       = "2006-01-02T15:04:05" // Python date format for json replies parsed as UTC
)

// Connection holds the details of the connection to the swift server.
//
// You need to provide UserName, ApiKey and AuthUrl when you create a
// connection then call Authenticate on it.
// 
// For reference some common AuthUrls looks like this:
//
//  Rackspace US        https://auth.api.rackspacecloud.com/v1.0
//  Rackspace UK        https://lon.auth.api.rackspacecloud.com/v1.0
//  Memset Memstore UK  https://auth.storage.memset.com/v1.0
type Connection struct {
	UserName   string // UserName for api
	ApiKey     string // Key for api access
	AuthUrl    string // Auth URL
	Retries    int    // Retries on error (default is 3)
	UserAgent  string // Http User agent (default goswift/1.0)
	storageUrl string
	authToken  string
	tr         *http.Transport
	client     *http.Client
}

// Error - all errors generated by this package are of this type.  Other error
// may be passed on from library functions though.
type Error struct {
	StatusCode int // HTTP status code if relevant or 0 if not
	Text       string
}

// Error satisfy the error interface.
func (e *Error) Error() string {
	return e.Text
}

// newError make a new error from a string.
func newError(StatusCode int, Text string) *Error {
	return &Error{
		StatusCode: StatusCode,
		Text:       Text,
	}
}

// newErrorf makes a new error from sprintf parameters.
func newErrorf(StatusCode int, Text string, Parameters ...interface{}) *Error {
	return newError(StatusCode, fmt.Sprintf(Text, Parameters...))
}

// errorMap defines http error codes to error mappings.
type errorMap map[int]error

var (
	// Specific Errors you might want to check for equality
	AuthorizationFailed = newError(401, "Authorization Failed")
	ContainerNotFound   = newError(404, "Container Not Found")
	ContainerNotEmpty   = newError(409, "Container Not Empty")
	ObjectNotFound      = newError(404, "Object Not Found")
	ObjectCorrupted     = newError(422, "Object Corrupted")

	// Mappings for authentication errors
	authErrorMap = errorMap{
		401: AuthorizationFailed,
	}

	// Mappings for container errors
	containerErrorMap = errorMap{
		404: ContainerNotFound,
		409: ContainerNotEmpty,
	}

	// Mappings for object errors
	objectErrorMap = errorMap{
		404: ObjectNotFound,
		422: ObjectCorrupted,
	}
)

// checkClose is used to check the return from Close in a defer
// statement.
func checkClose(c io.Closer, err *error) {
	cerr := c.Close()
	if *err == nil {
		*err = cerr
	}
}

// parseHeaders checks a response for errors and translates into
// standard errors if necessary.
func (c *Connection) parseHeaders(resp *http.Response, errorMap errorMap) error {
	if errorMap != nil {
		if err, ok := errorMap[resp.StatusCode]; ok {
			return err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newErrorf(resp.StatusCode, "HTTP Error: %d: %s", resp.StatusCode, resp.Status)
	}
	return nil
}

// readHeaders returns a Headers object from the http.Response.
//
// Logs a warning if receives multiple values for a key (which
// should never happen)
func readHeaders(resp *http.Response) Headers {
	headers := Headers{}
	for key, values := range resp.Header {
		headers[key] = values[0]
		if len(values) > 1 {
			log.Printf("swift: received multiple values for header %q", key)
		}
	}
	return headers
}

// Headers stores HTTP headers (can only have one of each header like Swift).
type Headers map[string]string

// Metadata stores account, container or object metadata.
type Metadata map[string]string

// Metadata gets the Metadata starting with the metaPrefix out of the Headers.
//
// The keys in the Metadata will be converted to lower case
func (h Headers) Metadata(metaPrefix string) Metadata {
	m := Metadata{}
	metaPrefix = http.CanonicalHeaderKey(metaPrefix)
	for key, value := range h {
		if strings.HasPrefix(key, metaPrefix) {
			metaKey := strings.ToLower(key[len(metaPrefix):])
			m[metaKey] = value
		}
	}
	return m
}

// AccountMetadata converts Headers from account to a Metadata.
//
// The keys in the Metadata will be converted to lower case.
func (h Headers) AccountMetadata() Metadata {
	return h.Metadata("X-Account-Meta-")
}

// ContainerMetadata converts Headers from container to a Metadata.
//
// The keys in the Metadata will be converted to lower case.
func (h Headers) ContainerMetadata() Metadata {
	return h.Metadata("X-Container-Meta-")
}

// ObjectMetadata converts Headers from object to a Metadata.
//
// The keys in the Metadata will be converted to lower case.
func (h Headers) ObjectMetadata() Metadata {
	return h.Metadata("X-Object-Meta-")
}

// Headers convert the Metadata starting with the metaPrefix into a
// Headers.
//
// The keys in the Metadata will be converted from lower case to http
// Canonical (see http.CanonicalHeaderKey).
func (m Metadata) Headers(metaPrefix string) Headers {
	h := Headers{}
	for key, value := range m {
		key = http.CanonicalHeaderKey(metaPrefix + key)
		h[key] = value
	}
	return h
}

// AccountHeaders converts the Metadata for the account.
func (m Metadata) AccountHeaders() Headers {
	return m.Headers("X-Account-Meta-")
}

// ContainerHeaders converts the Metadata for the container.
func (m Metadata) ContainerHeaders() Headers {
	return m.Headers("X-Container-Meta-")
}

// ObjectHeaders converts the Metadata for the object.
func (m Metadata) ObjectHeaders() Headers {
	return m.Headers("X-Object-Meta-")
}

// Authenticate connects to the Swift server.
func (c *Connection) Authenticate() (err error) {
	// Set defaults if not set
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.Retries == 0 {
		c.Retries = DefaultRetries
	}
	if c.tr == nil {
		c.tr = &http.Transport{
		//		TLSClientConfig:    &tls.Config{RootCAs: pool},
		//		DisableCompression: true,
		}
	}
	if c.client == nil {
		c.client = &http.Client{
			//		CheckRedirect: redirectPolicyFunc,
			Transport: c.tr,
		}
	}
	// Flush the keepalives connection - if we are
	// re-authenticating then stuff has gone wrong
	c.tr.CloseIdleConnections()
	var req *http.Request
	req, err = http.NewRequest("GET", c.AuthUrl, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("X-Auth-Key", c.ApiKey)
	req.Header.Set("X-Auth-User", c.UserName)
	var resp *http.Response
	resp, err = c.client.Do(req)
	if err != nil {
		return
	}
	defer func() {
		checkClose(resp.Body, &err)
		// Flush the auth connection - we don't want to keep
		// it open if keepalives were enabled
		c.tr.CloseIdleConnections()
	}()
	if err = c.parseHeaders(resp, authErrorMap); err != nil {
		return
	}
	c.storageUrl = resp.Header.Get("X-Storage-Url")
	c.authToken = resp.Header.Get("X-Auth-Token")
	if !c.Authenticated() {
		return newError(0, "Response didn't have storage url and auth token")
	}
	return nil
}

// UnAuthenticate removes the authentication from the Connection.
func (c *Connection) UnAuthenticate() {
	c.storageUrl = ""
	c.authToken = ""
}

// Authenticated returns a boolean to show if the current connection
// is authenticated.
//
// Doesn't actually check the credentials against the server.
func (c *Connection) Authenticated() bool {
	return c.storageUrl != "" && c.authToken != ""
}

// storageOpts contains parameters for Connection.storage.
type storageOpts struct {
	container  string
	objectName string
	operation  string
	parameters url.Values
	headers    Headers
	errorMap   errorMap
	noResponse bool
	body       io.Reader
	retries    int
}

// storage runs a remote command on a the storage url, returns a
// response, headers and possible error.
//
// operation is GET, HEAD etc
// container is the name of a container
// Any other parameters (if not None) are added to the storage url
//
// Returns a response or an error.  If response is returned then
// resp.Body.Close() must be called on it, unless noResponse is set in
// which case the body will be closed in this function
//
// This will Authenticate if necessary, and re-authenticate if it
// receives a 401 error which means the token has expired
func (c *Connection) storage(p storageOpts) (resp *http.Response, headers Headers, err error) {
	retries := p.retries
	if retries == 0 {
		retries = c.Retries
	}
	for {
		if !c.Authenticated() {
			err = c.Authenticate()
			if err != nil {
				return
			}
		}
		url := c.storageUrl
		if p.container != "" {
			url += "/" + p.container
			if p.objectName != "" {
				url += "/" + p.objectName
			}
		}
		if p.parameters != nil {
			encoded := p.parameters.Encode()
			if encoded != "" {
				url += "?" + encoded
			}
		}
		var req *http.Request
		req, err = http.NewRequest(p.operation, url, p.body)
		if err != nil {
			return
		}
		if p.headers != nil {
			for k, v := range p.headers {
				req.Header.Add(k, v)
			}
		}
		req.Header.Add("User-Agent", DefaultUserAgent)
		req.Header.Add("X-Auth-Token", c.authToken)
		// FIXME body of request?
		resp, err = c.client.Do(req)
		if err != nil {
			return
		}
		// Check to see if token has expired
		if resp.StatusCode == 401 && retries > 0 {
			_ = resp.Body.Close()
			c.UnAuthenticate()
			retries--
		} else {
			break
		}
	}

	if err = c.parseHeaders(resp, p.errorMap); err != nil {
		_ = resp.Body.Close()
		return nil, nil, err
	}
	headers = readHeaders(resp)
	if p.noResponse {
		err = resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return
}

// readLines reads the response into an array of strings.
//
// Closes the response when done
func readLines(resp *http.Response) (lines []string, err error) {
	defer checkClose(resp.Body, &err)
	reader := bufio.NewReader(resp.Body)
	buffer := bytes.NewBuffer(make([]byte, 0, 128))
	var part []byte
	var prefix bool
	for {
		if part, prefix, err = reader.ReadLine(); err != nil {
			break
		}
		buffer.Write(part)
		if !prefix {
			lines = append(lines, buffer.String())
			buffer.Reset()
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

// readJson reads the response into the json type passed in
//
// Closes the response when done
func readJson(resp *http.Response, result interface{}) (err error) {
	defer checkClose(resp.Body, &err)
	decoder := json.NewDecoder(resp.Body)
	return decoder.Decode(result)
}

/* ------------------------------------------------------------ */

// ContainersOpts is options for Containers() and ContainerNames()
type ContainersOpts struct {
	Limit     int     // For an integer value n, limits the number of results to at most n values.
	Marker    string  // Given a string value x, return object names greater in value than the specified marker.
	EndMarker string  // Given a string value x, return container names less in value than the specified marker.
	Headers   Headers // Any additional HTTP headers - can be nil
}

// parse the ContainerOpts
func (opts *ContainersOpts) parse() (url.Values, Headers) {
	v := url.Values{}
	var h Headers
	if opts != nil {
		if opts.Limit > 0 {
			v.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Marker != "" {
			v.Set("marker", opts.Marker)
		}
		if opts.EndMarker != "" {
			v.Set("end_marker", opts.EndMarker)
		}
		h = opts.Headers
	}
	return v, h
}

// ContainerNames returns a slice of names of containers in this account.
func (c *Connection) ContainerNames(opts *ContainersOpts) ([]string, error) {
	v, h := opts.parse()
	resp, _, err := c.storage(storageOpts{
		operation:  "GET",
		parameters: v,
		errorMap:   containerErrorMap,
		headers:    h,
	})
	if err != nil {
		return nil, err
	}
	lines, err := readLines(resp)
	return lines, err
}

// Container contains information about a container
type Container struct {
	Name  string // Name of the container
	Count int64  // Number of objects in the container
	Bytes int64  // Total number of bytes used in the container
}

// Containers returns a slice of structures with full information as
// described in Container.
func (c *Connection) Containers(opts *ContainersOpts) ([]Container, error) {
	v, h := opts.parse()
	v.Set("format", "json")
	resp, _, err := c.storage(storageOpts{
		operation:  "GET",
		parameters: v,
		errorMap:   containerErrorMap,
		headers:    h,
	})
	if err != nil {
		return nil, err
	}
	var containers []Container
	err = readJson(resp, &containers)
	return containers, err
}

/* ------------------------------------------------------------ */

// ObjectOpts is options for Objects() and ObjectNames()
type ObjectsOpts struct {
	Limit     int     // For an integer value n, limits the number of results to at most n values.
	Marker    string  // Given a string value x, return object names greater in value than the  specified marker.
	EndMarker string  // Given a string value x, return object names less in value than the specified marker
	Prefix    string  // For a string value x, causes the results to be limited to object names beginning with the substring x.
	Path      string  // For a string value x, return the object names nested in the pseudo path
	Delimiter rune    // For a character c, return all the object names nested in the container
	Headers   Headers // Any additional HTTP headers - can be nil
}

// parse reads values out of ObjectsOpts
func (opts *ObjectsOpts) parse() (url.Values, Headers) {
	v := url.Values{}
	var h Headers
	if opts != nil {
		if opts.Limit > 0 {
			v.Set("limit", strconv.Itoa(opts.Limit))
		}
		if opts.Marker != "" {
			v.Set("marker", opts.Marker)
		}
		if opts.EndMarker != "" {
			v.Set("end_marker", opts.EndMarker)
		}
		if opts.Prefix != "" {
			v.Set("prefix", opts.Prefix)
		}
		if opts.Path != "" {
			v.Set("path", opts.Path)
		}
		if opts.Delimiter != 0 {
			v.Set("delimiter", string(opts.Delimiter))
		}
		h = opts.Headers
	}
	return v, h
}

// ObjectNames returns a slice of names of objects in a given container.
func (c *Connection) ObjectNames(container string, opts *ObjectsOpts) ([]string, error) {
	v, h := opts.parse()
	resp, _, err := c.storage(storageOpts{
		container:  container,
		operation:  "GET",
		parameters: v,
		errorMap:   containerErrorMap,
		headers:    h,
	})
	if err != nil {
		return nil, err
	}
	return readLines(resp)
}

// Object contains information about an object
type Object struct {
	Name               string    `json:"name"`          // object name
	ContentType        string    `json:"content_type"`  // eg application/directory
	Bytes              int64     `json:"bytes"`         // size in bytes
	ServerLastModified string    `json:"last_modified"` // Last modified time, eg '2011-06-30T08:20:47.736680' as a string supplied by the server
	LastModified       time.Time // Last modified time converted to a time.Time
	Hash               string    `json:"hash"` // MD5 hash, eg "d41d8cd98f00b204e9800998ecf8427e"
	PseudoDirectory    bool      // Set when using delimiter to show that this directory object does not really exist
	SubDir             string    `json:"subdir"` // returned only when using delimiter to mark "pseudo directories"
}

// Objects returns a slice of Object with information about each
// object in the container.
//
// If Delimiter is set in the opts then PseudoDirectory may be set,
// with ContentType 'application/directory'.  These are not real
// objects but represent directories of objects which haven't had an
// object created for them.
func (c *Connection) Objects(container string, opts *ObjectsOpts) ([]Object, error) {
	v, h := opts.parse()
	v.Set("format", "json")
	resp, _, err := c.storage(storageOpts{
		container:  container,
		operation:  "GET",
		parameters: v,
		errorMap:   containerErrorMap,
		headers:    h,
	})
	if err != nil {
		return nil, err
	}
	var objects []Object
	err = readJson(resp, &objects)
	// Convert Pseudo directories and dates
	for i := range objects {
		object := &objects[i]
		if object.SubDir != "" {
			object.Name = object.SubDir
			object.PseudoDirectory = true
			object.ContentType = "application/directory"
		}
		if object.ServerLastModified != "" {
			// 2012-11-11T14:49:47.887250
			//
			// Remove fractional seconds if present. This
			// then keeps it consistent with Object
			// which can only return timestamps accurate
			// to 1 second
			//
			// The TimeFormat will parse fractional
			// seconds if desired though
			datetime := strings.SplitN(object.ServerLastModified, ".", 2)[0]
			object.LastModified, err = time.Parse(TimeFormat, datetime)
			if err != nil {
				return nil, err
			}
		}
	}
	return objects, err
}

// Account contains information about this account.
type Account struct {
	BytesUsed  int64 // total number of bytes used
	Containers int64 // total number of containers
	Objects    int64 // total number of objects
}

// getInt64FromHeader is a helper function to decode int64 from header.
func getInt64FromHeader(resp *http.Response, header string) (result int64, err error) {
	value := resp.Header.Get(header)
	result, err = strconv.ParseInt(value, 10, 64)
	if err != nil {
		err = newErrorf(0, "Bad Header '%s': '%s': %s", header, value, err)
	}
	return
}

// Account returns info about the account in an Account struct.
func (c *Connection) Account() (info Account, headers Headers, err error) {
	var resp *http.Response
	resp, headers, err = c.storage(storageOpts{
		operation:  "HEAD",
		errorMap:   containerErrorMap,
		noResponse: true,
	})
	if err != nil {
		return
	}
	// Parse the headers into a dict
	//
	//    {'Accept-Ranges': 'bytes',
	//     'Content-Length': '0',
	//     'Date': 'Tue, 05 Jul 2011 16:37:06 GMT',
	//     'X-Account-Bytes-Used': '316598182',
	//     'X-Account-Container-Count': '4',
	//     'X-Account-Object-Count': '1433'}
	if info.BytesUsed, err = getInt64FromHeader(resp, "X-Account-Bytes-Used"); err != nil {
		return
	}
	if info.Containers, err = getInt64FromHeader(resp, "X-Account-Container-Count"); err != nil {
		return
	}
	if info.Objects, err = getInt64FromHeader(resp, "X-Account-Object-Count"); err != nil {
		return
	}
	return
}

// AccountUpdate adds, replaces or remove account metadata.
//
// Add or update keys by mentioning them in the Headers.
//
// Remove keys by setting them to an empty string.
func (c *Connection) AccountUpdate(h Headers) error {
	_, _, err := c.storage(storageOpts{
		operation:  "POST",
		errorMap:   containerErrorMap,
		noResponse: true,
		headers:    h,
	})
	return err
}

// ContainerCreate creates a container.
//
// If you don't want to add Headers just pass in nil
//
// No error is returned if it already exists but the metadata if any will be updated.
func (c *Connection) ContainerCreate(container string, h Headers) error {
	_, _, err := c.storage(storageOpts{
		container:  container,
		operation:  "PUT",
		errorMap:   containerErrorMap,
		noResponse: true,
		headers:    h,
	})
	return err
}

// ContainerDelete deletes a container.
//
// May return ContainerDoesNotExist or ContainerNotEmpty
func (c *Connection) ContainerDelete(container string) error {
	_, _, err := c.storage(storageOpts{
		container:  container,
		operation:  "DELETE",
		errorMap:   containerErrorMap,
		noResponse: true,
	})
	return err
}

// Container returns info about a single container including any
// metadata in the headers.
func (c *Connection) Container(container string) (info Container, headers Headers, err error) {
	var resp *http.Response
	resp, headers, err = c.storage(storageOpts{
		container:  container,
		operation:  "HEAD",
		errorMap:   containerErrorMap,
		noResponse: true,
	})
	if err != nil {
		return
	}
	// Parse the headers into the struct
	info.Name = container
	if info.Bytes, err = getInt64FromHeader(resp, "X-Container-Bytes-Used"); err != nil {
		return
	}
	if info.Count, err = getInt64FromHeader(resp, "X-Container-Object-Count"); err != nil {
		return
	}
	return
}

// ContainerUpdate adds, replaces or removes container metadata.
//
// Add or update keys by mentioning them in the Metadata.
//
// Remove keys by setting them to an empty string.
//
// Container metadata can only be read with Container() not with Containers().
func (c *Connection) ContainerUpdate(container string, h Headers) error {
	_, _, err := c.storage(storageOpts{
		container:  container,
		operation:  "POST",
		errorMap:   containerErrorMap,
		noResponse: true,
		headers:    h,
	})
	return err
}

// ------------------------------------------------------------

// ObjectPut creates or updates the path in the container from
// contents.  contents should be an open io.Reader which will have all
// its contents read.
//
// This is a low level interface.
// 
// If checkHash is True then it will calculate the MD5 Hash of the
// file as it is being uploaded and check it against that returned
// from the server.  If it is wrong then it will return
// ObjectCorrupted.
// 
// If you know the MD5 hash of the object ahead of time then set the
// Hash parameter and it will be sent to the server (as an Etag
// header) and the server will check the MD5 itself after the upload,
// and this will return ObjectCorrupted if it is incorrect.
//
// If you don't want any error protection (not recommended) then set
// checkHash to false and Hash to "".
// 
// If contentType is set it will be used, otherwise one will be
// guessed from the name using the mimetypes module FIXME.
func (c *Connection) ObjectPut(container string, objectName string, contents io.Reader, checkHash bool, Hash string, contentType string, h Headers) (headers Headers, err error) {
	// FIXME I think this will do chunked transfer since we aren't providing a content length
	if contentType == "" {
		// http.DetectContentType FIXME
		contentType = "application/octet-stream" // FIXME
	}
	// Meta stuff
	extraHeaders := map[string]string{
		"Content-Type": contentType,
	}
	for key, value := range h {
		extraHeaders[key] = value
	}
	if Hash != "" {
		extraHeaders["Etag"] = Hash
		checkHash = false // the server will do it
	}
	hash := md5.New()
	var body io.Reader = contents
	if checkHash {
		body = io.TeeReader(contents, hash)
	}
	var resp *http.Response
	resp, headers, err = c.storage(storageOpts{
		container:  container,
		objectName: objectName,
		operation:  "PUT",
		headers:    extraHeaders,
		body:       body,
		noResponse: true,
		errorMap:   objectErrorMap,
	})
	if err != nil {
		return
	}
	if checkHash {
		receivedMd5 := strings.ToLower(resp.Header.Get("Etag"))
		calculatedMd5 := fmt.Sprintf("%x", hash.Sum(nil))
		if receivedMd5 != calculatedMd5 {
			err = ObjectCorrupted
			return
		}
	}
	return
}

// ObjectPutBytes creates an object from a []byte in a container.
//
// This is a simplified interface which checks the MD5.
func (c *Connection) ObjectPutBytes(container string, objectName string, contents []byte, contentType string) (err error) {
	buf := bytes.NewBuffer(contents)
	_, err = c.ObjectPut(container, objectName, buf, true, "", contentType, nil)
	return
}

// ObjectPutString creates an object from a string in a container.
//
// This is a simplified interface which checks the MD5
func (c *Connection) ObjectPutString(container string, objectName string, contents string, contentType string) (err error) {
	buf := strings.NewReader(contents)
	_, err = c.ObjectPut(container, objectName, buf, true, "", contentType, nil)
	return
}

// ObjectGet gets the object into the io.Writer contents.
// 
// Returns the headers of the response.
// 
// If checkHash is true then it will calculate the md5sum of the file
// as it is being received and check it against that returned from the
// server.  If it is wrong then it will return ObjectCorrupted.
//
// headers["Content-Type"] will give the content type if desired.
func (c *Connection) ObjectGet(container string, objectName string, contents io.Writer, checkHash bool, h Headers) (headers Headers, err error) {
	var resp *http.Response
	resp, headers, err = c.storage(storageOpts{
		container:  container,
		objectName: objectName,
		operation:  "GET",
		errorMap:   objectErrorMap,
		headers:    h,
	})
	if err != nil {
		return
	}
	defer checkClose(resp.Body, &err)
	hash := md5.New()
	var body io.Writer = contents
	if checkHash {
		body = io.MultiWriter(contents, hash)
	}
	var written int64
	written, err = io.Copy(body, resp.Body)
	if err != nil {
		return
	}

	// Check the MD5 sum if requested
	if checkHash {
		receivedMd5 := strings.ToLower(resp.Header.Get("Etag"))
		calculatedMd5 := fmt.Sprintf("%x", hash.Sum(nil))
		if receivedMd5 != calculatedMd5 {
			err = ObjectCorrupted
			return
		}
	}

	// Check to see we wrote the correct number of bytes
	if resp.Header.Get("Content-Length") != "" {
		var objectLength int64
		objectLength, err = getInt64FromHeader(resp, "Content-Length")
		if err != nil {
			return
		}
		if objectLength != written {
			err = ObjectCorrupted
			return
		}
	}

	return
}

// ObjectGetBytes returns an object as a []byte.
//
// This is a simplified interface which checks the MD5
func (c *Connection) ObjectGetBytes(container string, objectName string) (contents []byte, err error) {
	var buf bytes.Buffer
	_, err = c.ObjectGet(container, objectName, &buf, true, nil)
	contents = buf.Bytes()
	return
}

// ObjectGetString returns an object as a string.
//
// This is a simplified interface which checks the MD5
func (c *Connection) ObjectGetString(container string, objectName string) (contents string, err error) {
	var buf bytes.Buffer
	_, err = c.ObjectGet(container, objectName, &buf, true, nil)
	contents = buf.String()
	return
}

// ObjectDelete deletes the object.
//
// May return ObjectDoesNotExist if the object isn't found
func (c *Connection) ObjectDelete(container string, objectName string) error {
	_, _, err := c.storage(storageOpts{
		container:  container,
		objectName: objectName,
		operation:  "DELETE",
		errorMap:   objectErrorMap,
	})
	return err
}

// Object returns info about a single object including any metadata in the header.
//
// May return ObjectNotFound.
//
// Use headers.ObjectMetadata() to read the metadata in the Headers.
func (c *Connection) Object(container string, objectName string) (info Object, headers Headers, err error) {
	var resp *http.Response
	resp, headers, err = c.storage(storageOpts{
		container:  container,
		objectName: objectName,
		operation:  "HEAD",
		errorMap:   objectErrorMap,
		noResponse: true,
	})
	if err != nil {
		return
	}
	// Parse the headers into the struct
	// HTTP/1.1 200 OK
	// Date: Thu, 07 Jun 2010 20:59:39 GMT
	// Server: Apache
	// Last-Modified: Fri, 12 Jun 2010 13:40:18 GMT
	// ETag: 8a964ee2a5e88be344f36c22562a6486
	// Content-Length: 512000
	// Content-Type: text/plain; charset=UTF-8
	// X-Object-Meta-Meat: Bacon
	// X-Object-Meta-Fruit: Bacon
	// X-Object-Meta-Veggie: Bacon
	// X-Object-Meta-Dairy: Bacon
	info.Name = objectName
	info.ContentType = resp.Header.Get("Content-Type")
	if info.Bytes, err = getInt64FromHeader(resp, "Content-Length"); err != nil {
		return
	}
	info.ServerLastModified = resp.Header.Get("Last-Modified")
	if info.LastModified, err = time.Parse(http.TimeFormat, info.ServerLastModified); err != nil {
		return
	}
	info.Hash = resp.Header.Get("Etag")
	return
}

// ObjectUpdate adds, replaces or removes object metadata.
//
// Add or Update keys by mentioning them in the Metadata.  Use
// Metadata.ObjectHeaders and Headers.ObjectMetadata to convert your
// Metadata to and from normal HTTP headers.
//
// Remove keys by setting them to an empty string.
//
// Object metadata can only be read with Object() not with Objects().
//
// This can also be used to set headers not already assigned such as
// X-Delete-At or X-Delete-After for expiring objects.
//
// You cannot use this to change any of the object's other headers
// such as Content-Type, ETag, etc.
//
// Refer to copying an object when you need to update metadata or
// other headers such as Content-Type or CORS headers.
//
// May return ObjectNotFound.
func (c *Connection) ObjectUpdate(container string, objectName string, h Headers) error {
	_, _, err := c.storage(storageOpts{
		container:  container,
		objectName: objectName,
		operation:  "POST",
		errorMap:   objectErrorMap,
		noResponse: true,
		headers:    h,
	})
	return err
}
