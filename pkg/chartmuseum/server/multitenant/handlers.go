package multitenant

import (
	cm_repo "github.com/kubernetes-helm/chartmuseum/pkg/repo"

	"bytes"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	pathutil "path"
)

var (
	objectSavedResponse        = gin.H{"saved": true}
	objectDeletedResponse      = gin.H{"deleted": true}
	notFoundErrorResponse      = gin.H{"error": "not found"}
	badExtensionErrorResponse  = gin.H{"error": "unsupported file extension"}
	alreadyExistsErrorResponse = gin.H{"error": "file already exists"}
	healthCheckResponse        = gin.H{"healthy": true}
	warningHTML                = []byte(`<!DOCTYPE html>
<html>
<head>
<title>WARNING</title>
</head>
<body>
<h1>WARNING</h1>
<p>This ChartMuseum install is running in multitenancy mode.</p>
<p>This feature is still a work in progress, and is not considered stable.</p>
<p>Please run without the --multitenant flag to disable this.</p>
</body>
</html>
	`)
)

type (
	HTTPError struct {
		Status  int
		Message string
	}
)

type (
	packageOrProvenanceFile struct {
		filename string
		content  []byte
		field    string // file was extracted from this form field
	}
	filenameFromContentFn func([]byte) (string, error)
)

func (server *MultiTenantServer) defaultHandler(c *gin.Context) {
	c.Data(200, "text/html", warningHTML)
}

func (server *MultiTenantServer) getHealthCheckHandler(c *gin.Context) {
	c.JSON(200, healthCheckResponse)
}

func (server *MultiTenantServer) getIndexFileRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	log := server.Logger.ContextLoggingFn(c)
	indexFile, err := server.getIndexFile(log, repo)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message})
		return
	}
	c.Data(200, indexFileContentType, indexFile.Raw)
}

func (server *MultiTenantServer) getStorageObjectRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	filename := c.Param("filename")
	log := server.Logger.ContextLoggingFn(c)
	storageObject, err := server.getStorageObject(log, repo, filename)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message})
		return
	}
	c.Data(200, storageObject.ContentType, storageObject.Content)
}

func (server *MultiTenantServer) getAllChartsRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	log := server.Logger.ContextLoggingFn(c)
	indexFile, err := server.getIndexFile(log, repo)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message})
		return
	}
	c.JSON(200, indexFile.Entries)
}

func (server *MultiTenantServer) getChartRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	name := c.Param("name")
	log := server.Logger.ContextLoggingFn(c)
	indexFile, err := server.getIndexFile(log, repo)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message})
		return
	}
	chart := indexFile.Entries[name]
	if chart == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, chart)
}

func (server *MultiTenantServer) getChartVersionRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	name := c.Param("name")
	version := c.Param("version")
	if version == "latest" {
		version = ""
	}
	log := server.Logger.ContextLoggingFn(c)
	indexFile, err := server.getIndexFile(log, repo)
	if err != nil {
		c.JSON(err.Status, gin.H{"error": err.Message})
		return
	}
	chartVersion, getErr := indexFile.Get(name, version)
	if getErr != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, chartVersion)
}


func (server *MultiTenantServer) deleteChartVersionRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	name := c.Param("name")
	version := c.Param("version")
	filename := pathutil.Join(repo, cm_repo.ChartPackageFilenameFromNameVersion(name, version))
	server.Logger.Debugc(c, "Deleting package from storage",
		"package", filename,
	)
	err := server.StorageBackend.DeleteObject(filename)
	if err != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	provFilename := pathutil.Join(repo, cm_repo.ProvenanceFilenameFromNameVersion(name, version))
	server.StorageBackend.DeleteObject(provFilename) // ignore error here, may be no prov file
	c.JSON(200, gin.H{"deleted": true})
}

func (server *MultiTenantServer) postRequestHandler(c *gin.Context) {
	if c.ContentType() == "multipart/form-data" {
		server.postPackageAndProvenanceRequestHandler(c) // new route handling form-based chart and/or prov files
	} else {
		server.postPackageRequestHandler(c) // classic binary data, chart package only route
	}
}

func (server *MultiTenantServer) extractAndValidateFormFile(req *http.Request, field string, fnFromContent filenameFromContentFn) (*packageOrProvenanceFile, int, error) {
	file, header, _ := req.FormFile(field)
	var ppf *packageOrProvenanceFile
	if file == nil || header == nil {
		return ppf, 200, nil // field is not present
	}
	buf := bytes.NewBuffer(nil)
	_, err := io.Copy(buf, file)
	if err != nil {
		return ppf, 500, err // IO error
	}
	content := buf.Bytes()
	filename, err := fnFromContent(content)
	if err != nil {
		return ppf, 400, err // validation error (bad request)
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			return ppf, 409, fmt.Errorf("%s already exists", filename) // conflict
		}
	}
	return &packageOrProvenanceFile{filename, content, field}, 200, nil
}

func (server *MultiTenantServer) postPackageAndProvenanceRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	var ppFiles []*packageOrProvenanceFile

	type fieldFuncPair struct {
		field string
		fn    filenameFromContentFn
	}

	ffp := []fieldFuncPair{
		{server.ChartPostFormFieldName, cm_repo.ChartPackageFilenameFromContent},
		{server.ProvPostFormFieldName, cm_repo.ProvenanceFilenameFromContent},
	}

	for _, ff := range ffp {
		ppf, status, err := server.extractAndValidateFormFile(c.Request, ff.field, ff.fn)
		if err != nil {
			c.JSON(status, gin.H{"error": fmt.Sprintf("%s", err)})
			return
		}
		if ppf != nil {
			ppFiles = append(ppFiles, ppf)
		}
	}

	if len(ppFiles) == 0 {
		c.JSON(400, gin.H{"error": fmt.Sprintf(
			"no package or provenance file found in form fields %s and %s",
			server.ChartPostFormFieldName, server.ProvPostFormFieldName),
		})
		return
	}

	// At this point input is presumed valid, we now proceed to store it
	var storedFiles []*packageOrProvenanceFile
	for _, ppf := range ppFiles {
		server.Logger.Debugc(c, "Adding file to storage (form field)",
			"filename", ppf.filename,
			"field", ppf.field,
		)
		err := server.StorageBackend.PutObject(pathutil.Join(repo, ppf.filename), ppf.content)
		if err == nil {
			storedFiles = append(storedFiles, ppf)
		} else {
			// Clean up what's already been saved
			for _, ppf := range storedFiles {
				server.StorageBackend.DeleteObject(ppf.filename)
			}
			c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
			return
		}
	}
	c.JSON(201, objectSavedResponse)
}

func (server *MultiTenantServer) postPackageRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
		return
	}
	filename, err := cm_repo.ChartPackageFilenameFromContent(content)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(pathutil.Join(repo, filename))
		if err == nil {
			c.JSON(409, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugc(c,"Adding package to storage",
		"package", filename,
	)
	err = server.StorageBackend.PutObject(pathutil.Join(repo, filename), content)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s",err)})
		return
	}
	c.JSON(201, objectSavedResponse)
}

func (server *MultiTenantServer) postProvenanceFileRequestHandler(c *gin.Context) {
	repo := c.GetString("repo")
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
		return
	}
	filename, err := cm_repo.ProvenanceFilenameFromContent(content)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(pathutil.Join(repo, filename))
		if err == nil {
			c.JSON(409, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugc(c,"Adding provenance file to storage",
		"provenance_file", filename,
	)
	err = server.StorageBackend.PutObject(filename, content)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("%s", err)})
		return
	}
	c.JSON(201, objectSavedResponse)
}
