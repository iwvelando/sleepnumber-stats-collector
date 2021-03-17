package main

import (
	"flag"
	"fmt"
	_ "github.com/influxdata/influxdb1-client"
	influxdb "github.com/influxdata/influxdb1-client"
	"github.com/iwvelando/SleepIQ"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"net/url"
	"os"
	"strings"
	"time"
)

// Configuration represents a YAML-formatted config file
type Configuration struct {
	SleepIQUsername       string
	SleepIQPassword       string
	PollInterval          time.Duration
	InfluxDBURL           string
	InfluxDBPort          int64
	InfluxDBSkipVerifySsl bool
}

// Load a config file and return the Config struct
func LoadConfiguration(configPath string) (*Configuration, error) {
	viper.SetConfigFile(configPath)
	viper.AutomaticEnv()
	viper.SetConfigType("yml")

	err := viper.ReadInConfig()
	if err != nil {
		return nil, fmt.Errorf("error reading config file %s, %s", configPath, err)
	}

	var configuration Configuration
	err = viper.Unmarshal(&configuration)
	if err != nil {
		return nil, fmt.Errorf("unable to decode config into struct, %s", err)
	}

	return &configuration, nil
}

func BoolToInt(val bool) int8 {
	retVal := int8(0)
	if val {
		retVal = 1
	}
	return retVal
}

func main() {

	// Initialize the structured logger
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Println("{\"op\": \"main\", \"level\": \"fatal\", \"msg\": \"failed to initiate logger\"}")
		os.Exit(1)
	}
	defer logger.Sync()

	// Load the config file based on path provided via CLI or the default
	configLocation := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()
	config, err := LoadConfiguration(*configLocation)
	if err != nil {
		logger.Fatal("failed to load configuration",
			zap.String("op", "main"),
			zap.Error(err),
		)
	}

	// Initialize the InfluxDB connection
	influxDBAddr := fmt.Sprintf("%s:%d", config.InfluxDBURL, config.InfluxDBPort)
	host, err := url.Parse(influxDBAddr)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to parse InfluxDB URL %s", influxDBAddr),
			zap.String("op", "main"),
			zap.Error(err),
		)
	}
	influxConf := influxdb.Config{
		URL:       *host,
		UnsafeSsl: config.InfluxDBSkipVerifySsl,
	}
	influxConn, err := influxdb.NewClient(influxConf)
	if err != nil {
		logger.Fatal("failed to initialize InfluxDB connection",
			zap.String("op", "main"),
			zap.Error(err),
		)
	}

	// Initialize the SleepIQ client and login
	siq := sleepiq.New()

	_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
	if err != nil {
		logger.Fatal("failed to log into SleepIQ account",
			zap.String("op", "main"),
			zap.Error(err),
		)
	}

	for {

		pollStartTime := time.Now()

		// Query all beds
		beds, err := siq.Beds()
		if err != nil {
			logger.Error("failed to query beds",
				zap.String("op", "main"),
				zap.Error(err),
			)
			if strings.Contains(err.Error(), "Session is invalid") {
				logger.Info("refreshing login due to invalid session",
					zap.String("op", "main"),
				)
				_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
				if err != nil {
					logger.Fatal("failed to log into SleepIQ account",
						zap.String("op", "main"),
						zap.Error(err),
					)
				}
			}
			timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
			time.Sleep(time.Duration(timeRemaining))
			continue
		}

		// Query all beds via family status
		familyStatusBeds, err := siq.BedFamilyStatus()
		tsFamilyStatus := time.Now()
		if err != nil {
			logger.Error("failed to query family status beds",
				zap.String("op", "main"),
				zap.Error(err),
			)
			if strings.Contains(err.Error(), "Session is invalid") {
				logger.Info("refreshing login due to invalid session",
					zap.String("op", "main"),
				)
				_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
				if err != nil {
					logger.Fatal("failed to log into SleepIQ account",
						zap.String("op", "main"),
						zap.Error(err),
					)
				}
			}
			timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
			time.Sleep(time.Duration(timeRemaining))
			continue
		}

		// Form the output data
		data := make([]influxdb.Point, 0)

		for _, bed := range beds.Beds {

			foundation, err := siq.BedFoundationStatus(bed.BedID)
			if err != nil {
				logger.Error("failed to query bed foundation status",
					zap.String("op", "main"),
					zap.Error(err),
				)
				if strings.Contains(err.Error(), "Session is invalid") {
					logger.Info("refreshing login due to invalid session",
						zap.String("op", "main"),
					)
					_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
					if err != nil {
						logger.Fatal("failed to log into SleepIQ account",
							zap.String("op", "main"),
							zap.Error(err),
						)
					}
				}
				timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
				time.Sleep(time.Duration(timeRemaining))
				continue
			}
			tsFoundation := time.Now()
			data = append(data, influxdb.Point{
				Measurement: "bed_foundation_state",
				Tags: map[string]string{
					"size":       bed.Size,
					"name":       bed.Name,
					"generation": bed.Generation,
					"model":      bed.Model,
					"type":       foundation.Type,
				},
				Fields: map[string]interface{}{
					"is_moving":                     BoolToInt(foundation.IsMoving),
					"current_position_preset_right": foundation.CurrentPositionPresetRight,
					"current_position_preset_left":  foundation.CurrentPositionPresetLeft,
					"right_head_position":           foundation.RightHeadPosition,
					"left_head_position":            foundation.LeftHeadPosition,
					"right_foot_position":           foundation.RightFootPosition,
					"left_foot_position":            foundation.LeftFootPosition,
				},
				Time:      tsFoundation,
				Precision: "ns",
			})

			footwarmers, err := siq.BedFootWarmerStatus(bed.BedID)
			if err != nil {
				logger.Error("failed to query bed foundation status",
					zap.String("op", "main"),
					zap.Error(err),
				)
				if strings.Contains(err.Error(), "Session is invalid") {
					logger.Info("refreshing login due to invalid session",
						zap.String("op", "main"),
					)
					_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
					if err != nil {
						logger.Fatal("failed to log into SleepIQ account",
							zap.String("op", "main"),
							zap.Error(err),
						)
					}
				}
				timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
				time.Sleep(time.Duration(timeRemaining))
				continue
			}
			tsFootwarmers := time.Now()
			data = append(data, influxdb.Point{
				Measurement: "bed_footwarmers_state",
				Tags: map[string]string{
					"size":       bed.Size,
					"name":       bed.Name,
					"generation": bed.Generation,
					"model":      bed.Model,
				},
				Fields: map[string]interface{}{
					"foot_warming_status_left":  footwarmers.FootWarmingStatusLeft,
					"foot_warming_status_right": footwarmers.FootWarmingStatusRight,
				},
				Time:      tsFootwarmers,
				Precision: "ns",
			})

			for _, familyStatusBed := range familyStatusBeds.Beds {
				if familyStatusBed.BedID == bed.BedID {
					data = append(data, influxdb.Point{
						Measurement: "bed_sleeper_state",
						Tags: map[string]string{
							"size":       bed.Size,
							"name":       bed.Name,
							"generation": bed.Generation,
							"model":      bed.Model,
						},
						Fields: map[string]interface{}{
							"left_sleeper_is_in_bed":  BoolToInt(familyStatusBed.LeftSide.IsInBed),
							"right_sleeper_is_in_bed": BoolToInt(familyStatusBed.RightSide.IsInBed),
							"left_sleep_number":       familyStatusBed.LeftSide.SleepNumber,
							"right_sleep_number":      familyStatusBed.RightSide.SleepNumber,
							"left_pressure":           familyStatusBed.LeftSide.Pressure,
							"right_pressure":          familyStatusBed.RightSide.Pressure,
						},
						Time:      tsFamilyStatus,
						Precision: "ns",
					})
				}
			}
		}

		batchData := influxdb.BatchPoints{
			Points:          data,
			Database:        "sleepnumber",
			RetentionPolicy: "autogen",
		}

		_, err = influxConn.Write(batchData)
		if err != nil {
			logger.Warn("failed to write to InfluxDB",
				zap.String("op", "main"),
				zap.Error(err),
			)
		}

		timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
		time.Sleep(time.Duration(timeRemaining))

	}

}
