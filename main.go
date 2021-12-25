package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	influx "github.com/influxdata/influxdb-client-go/v2"
	influxAPI "github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/iwvelando/SleepIQ"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Configuration represents a YAML-formatted config file
type Configuration struct {
	SleepIQUsername string
	SleepIQPassword string
	PollInterval    time.Duration
	InfluxDB        InfluxDB
}

type InfluxDB struct {
	Address           string
	Username          string
	Password          string
	MeasurementPrefix string
	Database          string
	RetentionPolicy   string
	Token             string
	Organization      string
	Bucket            string
	SkipVerifySsl     bool
	FlushInterval     uint
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

type InfluxWriteConfigError struct{}

func (r *InfluxWriteConfigError) Error() string {
	return "must configure at least one of bucket or database/retention policy"
}

func InfluxConnect(config *Configuration) (influx.Client, influxAPI.WriteAPI, error) {
	var auth string
	if config.InfluxDB.Token != "" {
		auth = config.InfluxDB.Token
	} else if config.InfluxDB.Username != "" && config.InfluxDB.Password != "" {
		auth = fmt.Sprintf("%s:%s", config.InfluxDB.Username, config.InfluxDB.Password)
	} else {
		auth = ""
	}

	var writeDest string
	if config.InfluxDB.Bucket != "" {
		writeDest = config.InfluxDB.Bucket
	} else if config.InfluxDB.Database != "" && config.InfluxDB.RetentionPolicy != "" {
		writeDest = fmt.Sprintf("%s/%s", config.InfluxDB.Database, config.InfluxDB.RetentionPolicy)
	} else {
		return nil, nil, &InfluxWriteConfigError{}
	}

	if config.InfluxDB.FlushInterval == 0 {
		config.InfluxDB.FlushInterval = 30
	}

	options := influx.DefaultOptions().
		SetFlushInterval(1000 * config.InfluxDB.FlushInterval).
		SetTLSConfig(&tls.Config{
			InsecureSkipVerify: config.InfluxDB.SkipVerifySsl,
		})
	client := influx.NewClientWithOptions(config.InfluxDB.Address, auth, options)

	writeAPI := client.WriteAPI(config.InfluxDB.Organization, writeDest)

	return client, writeAPI, nil
}

func main() {

	// Load the config file based on path provided via CLI or the default
	configLocation := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()
	config, err := LoadConfiguration(*configLocation)
	if err != nil {
		log.WithFields(log.Fields{
			"op":    "main.LoadConfiguration",
			"error": err,
		}).Fatal("failed to load configuration")
	}

	// Initialize the SleepIQ client and login
	siq := sleepiq.New()

	_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
	if err != nil {
		log.WithFields(log.Fields{
			"op":    "main",
			"error": err,
		}).Fatal("failed to log into SleepIQ account")
	}

	// Initialize the InfluxDB connection
	influxClient, writeAPI, err := InfluxConnect(config)
	if err != nil {
		log.WithFields(log.Fields{
			"op":    "main",
			"error": err,
		}).Fatal("failed to initialize InfluxDB connection")
	}
	defer influxClient.Close()
	defer writeAPI.Flush()

	errorsCh := writeAPI.Errors()

	// Monitor InfluxDB write errors
	go func() {
		for err := range errorsCh {
			log.WithFields(log.Fields{
				"op":    "main",
				"error": err,
			}).Error("encountered error on writing to InfluxDB")
		}
	}()

	// Look for SIGTERM or SIGINT
	cancelCh := make(chan os.Signal, 1)
	signal.Notify(cancelCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		for {

			pollStartTime := time.Now()

			// Query all beds
			beds, err := siq.Beds()
			if err != nil {
				log.WithFields(log.Fields{
					"op":    "main",
					"error": err,
				}).Error("failed to query beds")
				if strings.Contains(err.Error(), "Session is invalid") {
					log.WithFields(log.Fields{
						"op": "main",
					}).Info("refreshing login due to invalid session")
					_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
					if err != nil {
						log.WithFields(log.Fields{
							"op":    "main",
							"error": err,
						}).Fatal("failed to log into SleepIQ account")
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
				log.WithFields(log.Fields{
					"op":    "main",
					"error": err,
				}).Error("failed to query family status beds")
				if strings.Contains(err.Error(), "Session is invalid") {
					log.WithFields(log.Fields{
						"op":    "main",
						"error": err,
					}).Info("refreshing login due to invalid session")
					_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
					if err != nil {
						log.WithFields(log.Fields{
							"op":    "main",
							"error": err,
						}).Fatal("failed to log into SleepIQ account")
					}
				}
				timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
				time.Sleep(time.Duration(timeRemaining))
				continue
			}

			for _, bed := range beds.Beds {

				foundation, err := siq.BedFoundationStatus(bed.BedID)
				if err != nil {
					log.WithFields(log.Fields{
						"op":    "main",
						"error": err,
					}).Error("failed to query bed foundation status")
					if strings.Contains(err.Error(), "Session is invalid") {
						log.WithFields(log.Fields{
							"op": "main",
						}).Info("refreshing login due to invalid session")
						_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
						if err != nil {
							log.WithFields(log.Fields{
								"op":    "main",
								"error": err,
							}).Fatal("failed to log into SleepIQ account")
						}
					}
					timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
					time.Sleep(time.Duration(timeRemaining))
					continue
				}
				tsFoundation := time.Now()
				data := influx.NewPoint(
					"bed_foundation_state",
					map[string]string{
						"size":       bed.Size,
						"name":       bed.Name,
						"generation": bed.Generation,
						"model":      bed.Model,
						"type":       foundation.Type,
					},
					map[string]interface{}{
						"is_moving":                     BoolToInt(foundation.IsMoving),
						"current_position_preset_right": foundation.CurrentPositionPresetRight,
						"current_position_preset_left":  foundation.CurrentPositionPresetLeft,
						"right_head_position":           foundation.RightHeadPosition,
						"left_head_position":            foundation.LeftHeadPosition,
						"right_foot_position":           foundation.RightFootPosition,
						"left_foot_position":            foundation.LeftFootPosition,
					},
					tsFoundation,
				)
				writeAPI.WritePoint(data)

				footwarmers, err := siq.BedFootWarmerStatus(bed.BedID)
				if err != nil {
					log.WithFields(log.Fields{
						"op":    "main",
						"error": err,
					}).Error("failed to query bed foundation status")
					if strings.Contains(err.Error(), "Session is invalid") {
						log.WithFields(log.Fields{
							"op": "main",
						}).Info("refreshing login due to invalid session")
						_, err = siq.Login(config.SleepIQUsername, config.SleepIQPassword)
						if err != nil {
							log.WithFields(log.Fields{
								"op":    "main",
								"error": err,
							}).Fatal("failed to log into SleepIQ account")
						}
					}
					timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
					time.Sleep(time.Duration(timeRemaining))
					continue
				}
				tsFootwarmers := time.Now()
				data = influx.NewPoint(
					"bed_footwarmers_state",
					map[string]string{
						"size":       bed.Size,
						"name":       bed.Name,
						"generation": bed.Generation,
						"model":      bed.Model,
					},
					map[string]interface{}{
						"foot_warming_status_left":  footwarmers.FootWarmingStatusLeft,
						"foot_warming_status_right": footwarmers.FootWarmingStatusRight,
					},
					tsFootwarmers,
				)
				writeAPI.WritePoint(data)

				for _, familyStatusBed := range familyStatusBeds.Beds {
					if familyStatusBed.BedID == bed.BedID {
						data := influx.NewPoint(
							"bed_sleeper_state",
							map[string]string{
								"size":       bed.Size,
								"name":       bed.Name,
								"generation": bed.Generation,
								"model":      bed.Model,
							},
							map[string]interface{}{
								"left_sleeper_is_in_bed":  BoolToInt(familyStatusBed.LeftSide.IsInBed),
								"right_sleeper_is_in_bed": BoolToInt(familyStatusBed.RightSide.IsInBed),
								"left_sleep_number":       familyStatusBed.LeftSide.SleepNumber,
								"right_sleep_number":      familyStatusBed.RightSide.SleepNumber,
								"left_pressure":           familyStatusBed.LeftSide.Pressure,
								"right_pressure":          familyStatusBed.RightSide.Pressure,
							},
							tsFamilyStatus,
						)
						writeAPI.WritePoint(data)
					}
				}
			}

			timeRemaining := config.PollInterval*time.Second - time.Since(pollStartTime)
			time.Sleep(time.Duration(timeRemaining))

		}
	}()

	sig := <-cancelCh
	log.WithFields(log.Fields{
		"op": "main",
	}).Info(fmt.Sprintf("caught signal %v, flushing data to InfluxDB", sig))
	writeAPI.Flush()
}
