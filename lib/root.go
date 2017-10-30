package lib

import (
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"text/template"

	"github.com/bitly/go-simplejson"
	"github.com/olekukonko/tablewriter"
	"gopkg.in/cheggaaa/pb.v2"
)

var pagePat = regexp.MustCompile(`<page width="(\d+)" height="(\d+)".+?>`)
var linePat = regexp.MustCompile(`<line .+?l="(\d+)" t="(\d+)" r="(\d+)" b="(\d+)">`)

const readmeTemplate = `
# archiscribe-corpus

This is the corpus repository for https://archiscribe.jbaiter.de.

The goal is to have as much diverse OCR ground truth for 19th Century German
prints as possible.

Currently the corpus contains {{.numLines}} lines from {{.numWorks}} works
published across {{.numYears}} years. Detailed statistics are available below.

## Statistics: Decades

{{.decadeTable}}

## Statistics: Years

{{.yearTable}}

## Statistics: Works

{{.worksTable}}
`

// IDCache is the global cache for suitable identifiers
var IDCache *IdentifierCache

// LineCache is the global cache for line images
var LineCache *LineImageCache

// OCRLine contains information about an OCR line
type OCRLine struct {
	ImageURL         string `json:"line"`
	PreviousImageURL string `json:"previous,omitempty"`
	NextImageURL     string `json:"next,omitempty"`
	Transcription    string `json:"transcription,omitempty"`
}

// TaskDefinition encodes a finished transcription along with author information
type TaskDefinition struct {
	Identifier string            `json:"id"`
	Lines      []OCRLine         `json:"lines"`
	Author     string            `json:"author,omitempty"`
	Comment    string            `json:"comment,omitempty"`
	Metadata   *simplejson.Json  `json:"metadata"`
	ResultChan chan SubmitResult `json:"-"`
}

// SubmitResult holds the result of a submission
type SubmitResult struct {
	CommitSha string `json:"commit,omitempty"`
	Error     error  `json:"error,omitempty"`
}

// ProgressReader wraps another reader and exposes progress information
type ProgressReader struct {
	proxiedReader io.Reader
	BytesRead     int64
}

// NewProgressReader creates a new ProgressReader from a given Reader
func NewProgressReader(proxied io.Reader) *ProgressReader {
	return &ProgressReader{proxied, 0}
}

func (r *ProgressReader) Read(p []byte) (n int, err error) {
	n, err = r.proxiedReader.Read(p)
	if n > 0 {
		r.BytesRead += int64(n)
	} else {
		r.BytesRead += int64(len(p))
	}
	return n, err
}

// CacheLines caches three volumes for each year to disk
func CacheLines(cachePath string, year int, printProgress bool) string {
	yearPath := path.Join(cachePath, strconv.Itoa(year))
OuterCache:
	for {
		entry := IDCache.Random(year)
		ident := entry.Identifier
		isFrak, _ := IsFraktur(ident)
		if !isFrak {
			continue
		}
		progChan, lineChan, err := FetchLines(ident)
		if err != nil {
			log.Printf("Error while getting OCR for %s: %+v", ident, err)
			continue
		}
		log.Printf("Caching lines for %d from %s", year, ident)
		var progBar *pb.ProgressBar
		if printProgress {
			progBar = pb.New(100)
			progBar.SetWidth(80)
			progBar.Start()
		}
		for {
			select {
			case prog := <-progChan:
				if prog.Error != nil {
					log.Printf("Error while getting lines for %s: %+v", ident, prog.Error)
					continue OuterCache
				} else if printProgress {
					progBar.SetCurrent(int64(prog.Progress * 100))
				}
			case lines := <-lineChan:
				filePath := path.Join(yearPath, ident+".json")
				lineJSON, _ := json.Marshal(lines)
				ioutil.WriteFile(filePath, lineJSON, 0644)
				if printProgress {
					progBar.Finish()
				}
				return filePath
			}
		}
	}
}

// TODO: Holy shit, maybe that whole caching thing is completely unneccessary
//       and the slowness was just due to Python >_<
func cacheWatcher(basePath string) (map[int]chan string, error) {
	cacheChannels := map[int]chan string{}
	cacheFiles := map[int][]string{}
	//bufferedIds := map[string]bool{}
	yearDirs, err := ioutil.ReadDir(basePath)
	if err != nil {
		return nil, err
	}
	for _, yearDir := range yearDirs {
		if !yearDir.IsDir() {
			continue
		}
		year, _ := strconv.Atoi(yearDir.Name())
		yearPath := path.Join(basePath, yearDir.Name())
		dirContent, _ := ioutil.ReadDir(yearPath)
		cacheFiles[year] = make([]string, len(dirContent))
		for _, f := range dirContent {
			if f.IsDir() || path.Ext(f.Name()) != ".json" {
				continue
			}
			cacheFiles[year] = append(cacheFiles[year], path.Join(yearPath, f.Name()))
		}
		// Fill up cache
		for len(cacheFiles[year]) < 3 {
			cacheFiles[year] = append(cacheFiles[year], CacheLines(basePath, year, true))
		}
	}
	//go func() {
	// TODO: Create SelectCases with the remaining line
	// TODO: Select on the cases, when one is selected fetch another item
	//		 for that year, update the case and continue selecting
	//}()
	return cacheChannels, nil
	/*
		cacheMap := map[chan []OCRLine]int{}
		cases := make([]reflect.SelectCase, len(cacheMap))
		for year := range idCache {
			ch := make(chan []OCRLine)
			cacheMap[ch] = year
			cases = append(cases, reflect.SelectCase{
				Dir: reflect.SelectSend, Chan: reflect.ValueOf(ch),
				Send: reflect.New(nil)}) // TODO: Load from next cached lines file
		}
		for {
			// Wait for channels to become free
			chosen, recv, recvOk := reflect.Select(cases)
			year := cacheMap[cases[chosen].Chan.Close]

		}
	*/
}

func createReadme(repoPath string) string {
	metaFiles, _ := filepath.Glob(fmt.Sprintf("%s/transcriptions/*/*.json", repoPath))
	// TODO: Handle error
	sort.Strings(metaFiles)
	numLinesTotal := 0
	yearCount := map[int]int{}
	decadeCount := map[int]int{}
	metaRows := [][]string{}
	for _, metaPath := range metaFiles {
		fp, err := os.Open(metaPath)
		if err != nil {
			log.Printf("Could not read meta file %s: %+v", metaPath, err)
			continue
		}
		meta, err := simplejson.NewFromReader(fp)
		if err != nil {
			log.Printf("Could not parse meta file %s: %+v", metaPath, err)
			continue
		}
		numLines := len(meta.Get("lines").MustMap())
		numLinesTotal += numLines
		year, _ := strconv.Atoi(meta.Get("year").MustString())
		decade := (year / 10) * 10
		yearCount[year] += numLines
		decadeCount[decade] += numLines
		ident := meta.Get("identifier").MustString()
		archiveLink := fmt.Sprintf(
			"[%s](http://archive.org/details/%s)", ident, ident)
		manifestLink := fmt.Sprintf(
			"[Manifest](https://iiif.archivelab.org/iiif/%s/manifest.json)", ident)
		miradorLink := fmt.Sprintf(
			"[Mirador](ttps://iiif.archivelab.org/iiif/%s)", ident)
		metaRows = append(metaRows, []string{
			meta.Get("title").MustString(), meta.Get("date").MustString(),
			archiveLink, fmt.Sprintf("%s/%s", manifestLink, miradorLink)})
	}

	var yearsTable bytes.Buffer
	var years []int
	for k := range yearCount {
		years = append(years, k)
	}
	sort.Ints(years)
	t := tablewriter.NewWriter(&yearsTable)
	t.SetAutoFormatHeaders(false)
	t.SetHeader([]string{"Year", "# lines"})
	t.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
	t.SetCenterSeparator("|")
	for _, year := range years {
		t.Append([]string{strconv.Itoa(year), strconv.Itoa(yearCount[year])})
	}
	t.Render()

	var decadesTable bytes.Buffer
	var decades []int
	for k := range decadeCount {
		decades = append(decades, k)
	}
	sort.Ints(decades)
	t = tablewriter.NewWriter(&decadesTable)
	t.SetAutoFormatHeaders(false)
	t.SetHeader([]string{"Decade", "# lines"})
	t.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
	t.SetCenterSeparator("|")
	for _, decade := range decades {
		t.Append([]string{strconv.Itoa(decade), strconv.Itoa(decadeCount[decade])})
	}
	t.Render()

	var metaTable bytes.Buffer
	t = tablewriter.NewWriter(&metaTable)
	t.SetAutoFormatHeaders(false)
	t.SetAutoWrapText(false)
	t.SetHeader([]string{"Title", "Date", "Archive.org", "IIIF"})
	t.SetBorders(tablewriter.Border{Left: true, Top: false, Right: true, Bottom: false})
	t.SetCenterSeparator("|")
	t.AppendBulk(metaRows)
	t.Render()

	var out bytes.Buffer
	tmpl := template.Must(template.New("README.md").Parse(readmeTemplate))
	tmpl.Execute(&out, map[string]string{
		"numLines":    strconv.Itoa(numLinesTotal),
		"numWorks":    strconv.Itoa(len(metaFiles)),
		"numYears":    strconv.Itoa(len(years)),
		"decadeTable": decadesTable.String(),
		"yearTable":   yearsTable.String(),
		"worksTable":  metaTable.String(),
	})
	return out.String()
}

// InitCache initializes global identifier cache
func InitCache() {
	cacheDir, isSet := os.LookupEnv("ARCHISCRIBE_CACHE")
	if !isSet {
		cacheDir = "./cache"
	}
	cacheDir, _ = filepath.Abs(cacheDir)
	dirStat, err := os.Stat(cacheDir)
	if os.IsNotExist(err) {
		os.MkdirAll(cacheDir, 0755)
	} else if !dirStat.IsDir() {
		log.Panicf("Cache directory '%s' is not a directory!", cacheDir)
	} else if err != nil {
		log.Panicf("Error setting up cache directory: %v", err)
	}
	LineCache = NewLineImageCache(cacheDir)
	idCacheFile := filepath.Join(cacheDir, "identifiers.json")
	if _, err := os.Stat(idCacheFile); err != nil {
		fmt.Println("Caching identifiers...")
		cache, err := CacheIdentifiers(idCacheFile)
		if err != nil {
			panic(err)
		}
		IDCache = cache
	} else {
		IDCache = LoadIdentifierCache(idCacheFile)
	}
}

// Sha1Digest generates the SHA1 digest for the given data
func Sha1Digest(inp []byte) string {
	hash := sha1.New()
	hash.Write(inp)
	return fmt.Sprintf("%x", hash.Sum(nil))[:8]
}

// MakeLineIdentifier returns the unique identifier for a line
func MakeLineIdentifier(volumeID string, line OCRLine) string {
	shaHash := Sha1Digest([]byte(line.ImageURL))
	return fmt.Sprintf("%s_%s", volumeID, shaHash)
}
