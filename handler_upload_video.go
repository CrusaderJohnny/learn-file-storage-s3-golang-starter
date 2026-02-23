package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	videoIdString := r.PathValue("videoID")
	videoId, err := uuid.Parse(videoIdString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid ID", err)
		return
	}
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "no valid token found", err)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "could not validate token", err)
		return
	}
	video, err := cfg.db.GetVideo(videoId)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "video not found", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "unauthorized access", errors.New("unauthorized access"))
		return
	}
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue forming file from video: ", err)
		return
	}
	defer file.Close()
	videoType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue parsing media type", err)
		return
	}
	if videoType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "media type must be video/mp4", nil)
		return
	}
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue creating temp file", err)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue copying video to temporary upload location", err)
		return
	}
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue getting video aspect ratio", err)
		return
	}
	aspectFormat := "other"
	if aspectRatio == "16:9" {
		aspectFormat = "landscape"
	}
	if aspectRatio == "9:16" {
		aspectFormat = "portrait"
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue resetting pointer to beginning", err)
		return
	}
	saveName := make([]byte, 32)
	_, err = rand.Read(saveName)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue generating random filename", err)
		return
	}
	key := aspectFormat + "/" + hex.EncodeToString(saveName) + ".mp4"
	saveFileToBucket, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue uploading video to fast start", err)
		return
	}
	defer os.Remove(saveFileToBucket)
	fileToSave, err := os.Open(saveFileToBucket)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue uploading video to fast start", err)
		return
	}
	defer fileToSave.Close()

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        fileToSave,
		ContentType: aws.String(videoType),
	})
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue uploading video to s3", err)
		return
	}

	dataURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
	video.VideoURL = &dataURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "issue updating video", err)
		return
	}
	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	type aspectRatio struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		}
	}
	returnedRatios := aspectRatio{}
	err = json.Unmarshal(stdout.Bytes(), &returnedRatios)
	if err != nil {
		return "", err
	}
	if len(returnedRatios.Streams) == 0 {
		return "", errors.New("no aspect ratios found")
	}
	ratio := float64(returnedRatios.Streams[0].Width) / float64(returnedRatios.Streams[0].Height)
	roundedRatio := math.Round(ratio*100) / 100
	if roundedRatio == 1.78 {
		return "16:9", nil
	}
	if roundedRatio == 0.56 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filepath string) (string, error) {
	outputPath := filepath + ".processing"
	ffmpegCommand := exec.Command("ffmpeg", "-i", filepath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	err := ffmpegCommand.Run()
	if err != nil {
		return "", err
	}
	return outputPath, nil
}
