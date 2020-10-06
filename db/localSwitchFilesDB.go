package db

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/giwty/switch-library-manager/fileio"
	"github.com/giwty/switch-library-manager/settings"
	"github.com/giwty/switch-library-manager/switchfs"
	"go.uber.org/zap"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	. "time"
)

var (
	versionRegex = regexp.MustCompile(`\[[vV]?(?P<version>[0-9]{1,10})]`)
	titleIdRegex = regexp.MustCompile(`\[(?P<titleId>[A-Z,a-z0-9]{16})]`)
)

const (
	REASON_UNSUPPORTED_TYPE = iota
	REASON_DUPLICATE
	REASON_OLD_UPDATE
	REASON_UNRECOGNISED
	REASON_MALFORMED_FILE
)

type LocalSwitchDBManager struct {
	db *bolt.DB
}

func NewLocalSwitchDBManager(baseFolder string) (*LocalSwitchDBManager, error) {
	// Open the my.db data file in your current directory.
	// It will be created if it doesn't exist.
	db, err := bolt.Open(filepath.Join(baseFolder, "slm.db"), 0600, &bolt.Options{Timeout: 1 * Second})
	if err != nil {
		log.Fatal(err)
		return nil, err
	}

	//get DB version
	appVersion := ""
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("deep-scan"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte("app_version"))
		if v == nil {
			err := db.Update(func(tx *bolt.Tx) error {
				err = tx.DeleteBucket([]byte("deep-scan"))
				return err
			})
			return err
		}
		d := gob.NewDecoder(bytes.NewReader(v))

		err = d.Decode(&appVersion)
		if err != nil {
			return err
		}

		return nil
	})

	return &LocalSwitchDBManager{db: db}, nil
}

func (ldb *LocalSwitchDBManager) Close() {
	ldb.db.Close()
}

type ExtendedFileInfo struct {
	Info       os.FileInfo
	BaseFolder string
}

type SwitchFileInfo struct {
	ExtendedInfo ExtendedFileInfo
	Metadata     *switchfs.ContentMetaAttributes
}

type SwitchGameFiles struct {
	File         SwitchFileInfo
	BaseExist    bool
	Updates      map[int]SwitchFileInfo
	Dlc          map[string]SwitchFileInfo
	MultiContent bool
	LatestUpdate int
	IsSplit      bool
}

type SkippedFile struct {
	ReasonCode     int
	ReasonText     string
	AdditionalInfo string
}

type LocalSwitchFilesDB struct {
	TitlesMap map[string]*SwitchGameFiles
	Skipped   map[ExtendedFileInfo]SkippedFile
	NumFiles  int
}

func (ldb *LocalSwitchDBManager) CreateLocalSwitchFilesDB(folders []string, progress ProgressUpdater, recursive bool) (*LocalSwitchFilesDB, error) {
	titles := map[string]*SwitchGameFiles{}
	skipped := map[ExtendedFileInfo]SkippedFile{}
	files := []ExtendedFileInfo{}
	for i, folder := range folders {
		err := scanFolder(folder, recursive, &files, progress)
		if progress != nil {
			progress.UpdateProgress(i+1, len(folders)+1, "scanning files in "+folder)
		}
		if err != nil {
			continue
		}
	}

	ldb.processLocalFiles(files, progress, titles, skipped)

	if progress != nil {
		progress.UpdateProgress(len(files), len(files), "Complete")
	}

	return &LocalSwitchFilesDB{TitlesMap: titles, Skipped: skipped, NumFiles: len(files)}, nil
}

func scanFolder(folder string, recursive bool, files *[]ExtendedFileInfo, progress ProgressUpdater) error {
	filepath.Walk(folder, func(path string, info os.FileInfo, err error) error {
		if path == folder {
			return nil
		}
		if err != nil {
			zap.S().Error("Error while scanning folders", err)
			return nil
		}

		if info.IsDir() {
			return nil
		}

		//skip mac hidden files
		if info.Name()[0:1] == "." {
			return nil
		}
		base := path[0 : len(path)-len(info.Name())]
		if strings.TrimSuffix(base, string(os.PathSeparator)) != strings.TrimSuffix(folder, string(os.PathSeparator)) &&
			!recursive {
			return nil
		}
		if progress != nil {
			progress.UpdateProgress(-1, -1, "scanning "+info.Name())
		}
		*files = append(*files, ExtendedFileInfo{Info: info, BaseFolder: base})

		return nil
	})
	return nil
}

func (ldb *LocalSwitchDBManager) processLocalFiles(files []ExtendedFileInfo,
	progress ProgressUpdater,
	titles map[string]*SwitchGameFiles,
	skipped map[ExtendedFileInfo]SkippedFile) {
	ind := 0
	total := len(files)
	for _, file := range files {
		ind += 1
		if progress != nil {
			progress.UpdateProgress(ind, total, "process:"+file.Info.Name())
		}

		//scan sub-folders if flag is present
		filePath := filepath.Join(file.BaseFolder, file.Info.Name())
		if file.Info.IsDir() {
			continue
		}

		fileName := strings.ToLower(file.Info.Name())
		isSplit := false

		if partNum, err := strconv.Atoi(fileName[len(fileName)-2:]); err == nil {
			if partNum == 0 {
				isSplit = true
			} else {
				continue
			}

		}

		//only handle NSZ and NSP files

		if !isSplit &&
			!strings.HasSuffix(fileName, "xci") &&
			!strings.HasSuffix(fileName, "nsp") &&
			!strings.HasSuffix(fileName, "nsz") &&
			!strings.HasSuffix(fileName, "xcz") {
			skipped[file] = SkippedFile{ReasonCode: REASON_UNSUPPORTED_TYPE, ReasonText: "file type is not supported"}
			continue
		}

		contentMap, err := ldb.getGameMetadata(file, filePath, skipped)

		if err != nil {
			if _, ok := skipped[file]; !ok {
				skipped[file] = SkippedFile{ReasonText: "unable to determine title-Id / version - " + err.Error(), ReasonCode: REASON_UNRECOGNISED}
			}
			continue
		}

		for _, metadata := range contentMap {

			idPrefix := metadata.TitleId[0 : len(metadata.TitleId)-4]

			multiContent := len(contentMap) > 1
			switchTitle := &SwitchGameFiles{
				MultiContent: multiContent,
				Updates:      map[int]SwitchFileInfo{},
				Dlc:          map[string]SwitchFileInfo{},
				BaseExist:    false,
				IsSplit:      isSplit,
				LatestUpdate: 0,
			}
			if t, ok := titles[idPrefix]; ok {
				switchTitle = t
			}
			titles[idPrefix] = switchTitle

			//process Updates
			if strings.HasSuffix(metadata.TitleId, "800") {
				metadata.Type = "Update"

				if update, ok := switchTitle.Updates[metadata.Version]; ok {
					skipped[file] = SkippedFile{ReasonCode: REASON_DUPLICATE, ReasonText: "duplicate update file (" + update.ExtendedInfo.Info.Name() + ")"}
					zap.S().Warnf("-->Duplicate update file found [%v] and [%v]", update.ExtendedInfo.Info.Name(), file.Info.Name())
					continue
				}
				switchTitle.Updates[metadata.Version] = SwitchFileInfo{ExtendedInfo: file, Metadata: metadata}
				if metadata.Version > switchTitle.LatestUpdate {
					if switchTitle.LatestUpdate != 0 {
						skipped[switchTitle.Updates[switchTitle.LatestUpdate].ExtendedInfo] = SkippedFile{ReasonCode: REASON_OLD_UPDATE, ReasonText: "old update file, newer update exist locally"}
					}
					switchTitle.LatestUpdate = metadata.Version
				} else {
					skipped[file] = SkippedFile{ReasonCode: REASON_OLD_UPDATE, ReasonText: "old update file, newer update exist locally"}
				}
				continue
			}

			//process base
			if strings.HasSuffix(metadata.TitleId, "000") {
				metadata.Type = "Base"
				if switchTitle.BaseExist {
					skipped[file] = SkippedFile{ReasonCode: REASON_DUPLICATE, ReasonText: "duplicate base file (" + switchTitle.File.ExtendedInfo.Info.Name() + ")"}
					zap.S().Warnf("-->Duplicate base file found [%v] and [%v]", file.Info.Name(), switchTitle.File.ExtendedInfo.Info.Name())
					continue
				}
				switchTitle.File = SwitchFileInfo{ExtendedInfo: file, Metadata: metadata}
				switchTitle.BaseExist = true

				continue
			}

			if dlc, ok := switchTitle.Dlc[metadata.TitleId]; ok {
				if metadata.Version < dlc.Metadata.Version {
					skipped[file] = SkippedFile{ReasonCode: REASON_OLD_UPDATE, ReasonText: "old DLC file, newer version exist locally"}
					zap.S().Warnf("-->Old DLC file found [%v] and [%v]", file.Info.Name(), dlc.ExtendedInfo.Info.Name())
					continue
				} else if metadata.Version == dlc.Metadata.Version {
					skipped[file] = SkippedFile{ReasonCode: REASON_DUPLICATE, ReasonText: "duplicate DLC file (" + dlc.ExtendedInfo.Info.Name() + ")"}
					zap.S().Warnf("-->Duplicate DLC file found [%v] and [%v]", file.Info.Name(), dlc.ExtendedInfo.Info.Name())
					continue
				}
			}
			//not an update, and not main TitleAttributes, so treat it as a DLC
			metadata.Type = "DLC"
			switchTitle.Dlc[metadata.TitleId] = SwitchFileInfo{ExtendedInfo: file, Metadata: metadata}
		}
	}

}

func (ldb *LocalSwitchDBManager) ClearDB() error {
	err := ldb.db.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket([]byte("deep-scan"))
		return err
	})
	return err
}

func (ldb *LocalSwitchDBManager) getGameMetadata(file ExtendedFileInfo,
	filePath string,
	skipped map[ExtendedFileInfo]SkippedFile) (map[string]*switchfs.ContentMetaAttributes, error) {

	var metadata map[string]*switchfs.ContentMetaAttributes = nil
	keys, _ := settings.SwitchKeys()
	var err error
	fileKey := filePath + "|" + file.Info.Name() + "|" + strconv.Itoa(int(file.Info.Size()))
	if keys != nil && keys.GetKey("header_key") != "" {
		err = ldb.db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("deep-scan"))
			if b == nil {
				return nil
			}
			v := b.Get([]byte(fileKey))
			if v == nil {
				return nil
			}
			d := gob.NewDecoder(bytes.NewReader(v))

			// Decoding the serialized data
			err = d.Decode(&metadata)
			if err != nil {
				return err
			}
			return nil
		})

		if err != nil {
			zap.S().Warnf("%v", err)
		}

		if metadata != nil {
			return metadata, nil
		}

		fileName := strings.ToLower(file.Info.Name())
		if strings.HasSuffix(fileName, "nsp") ||
			strings.HasSuffix(fileName, "nsz") {
			metadata, err = switchfs.ReadNspMetadata(filePath)
			if err != nil {
				skipped[file] = SkippedFile{ReasonCode: REASON_MALFORMED_FILE, ReasonText: fmt.Sprintf("failed to read NSP [reason: %v]", err)}
				zap.S().Errorf("[file:%v] failed to read NSP [reason: %v]\n", file.Info.Name(), err)
			}
		} else if strings.HasSuffix(fileName, "xci") ||
			strings.HasSuffix(fileName, "xcz") {
			metadata, err = switchfs.ReadXciMetadata(filePath)
			if err != nil {
				skipped[file] = SkippedFile{ReasonCode: REASON_MALFORMED_FILE, ReasonText: fmt.Sprintf("failed to read NSP [reason: %v]", err)}
				zap.S().Errorf("[file:%v] failed to read file [reason: %v]\n", file.Info.Name(), err)
			}
		} else if strings.HasSuffix(fileName, "00") {
			metadata, err = fileio.ReadSplitFileMetadata(filePath)
			if err != nil {
				skipped[file] = SkippedFile{ReasonCode: REASON_MALFORMED_FILE, ReasonText: fmt.Sprintf("failed to read split files [reason: %v]", err)}
				zap.S().Errorf("[file:%v] failed to read NSP [reason: %v]\n", file.Info.Name(), err)
			}
		}
	}

	if metadata != nil {
		err = ldb.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte("deep-scan"))
			if b == nil {
				b, err = tx.CreateBucket([]byte("deep-scan"))
				if b == nil || err != nil {
					return fmt.Errorf("create bucket: %s", err)
				}
				err := b.Put([]byte("app_version"), []byte(settings.SLM_VERSION))
				if err != nil {
					zap.S().Warnf("failed to save app_version - %v", err)
				}
			}
			var bytesBuff bytes.Buffer
			encoder := gob.NewEncoder(&bytesBuff)
			err = encoder.Encode(metadata)
			if err != nil {
				return err
			}
			err := b.Put([]byte(fileKey), bytesBuff.Bytes())
			return err
		})
		if err != nil {
			zap.S().Warnf("%v", err)
		}
		return metadata, nil
	}

	//fallback to parse data from filename

	//parse title id
	titleId, _ := parseTitleIdFromFileName(file.Info.Name())
	version, _ := parseVersionFromFileName(file.Info.Name())

	if titleId == nil || version == nil {
		return nil, errors.New("unable to determine titileId / version")
	}
	metadata = map[string]*switchfs.ContentMetaAttributes{}
	metadata[*titleId] = &switchfs.ContentMetaAttributes{TitleId: *titleId, Version: *version}

	return metadata, nil
}

func parseVersionFromFileName(fileName string) (*int, error) {
	res := versionRegex.FindStringSubmatch(fileName)
	if len(res) != 2 {
		return nil, errors.New("failed to parse name - no version id found")
	}
	ver, err := strconv.Atoi(res[1])
	if err != nil {
		return nil, errors.New("failed to parse name - no version id found")
	}
	return &ver, nil
}

func parseTitleIdFromFileName(fileName string) (*string, error) {
	res := titleIdRegex.FindStringSubmatch(fileName)

	if len(res) != 2 {
		return nil, errors.New("failed to parse name - no title id found")
	}
	titleId := strings.ToLower(res[1])
	return &titleId, nil
}

func ParseTitleNameFromFileName(fileName string) string {
	ind := strings.Index(fileName, "[")
	if ind != -1 {
		return fileName[:ind]
	}
	return fileName
}
