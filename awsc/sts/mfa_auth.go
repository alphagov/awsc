package sts

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	ini "gopkg.in/ini.v1"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/defaults"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
)

func promptMFAToken() (string, error) {
	fmt.Print("MFA token: ")
	byteToken, err := terminal.ReadPassword(int(syscall.Stdin))
	fmt.Println("******")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(byteToken)), nil
}

func loadSession(file string) (*sts.Credentials, error) {
	file = file + ".json"
	data, err := ioutil.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	credentials := &sts.Credentials{}
	err = json.Unmarshal(data, credentials)
	if err != nil {
		return nil, err
	}
	if credentials.Expiration.Before(time.Now()) {
		return nil, nil
	}
	return credentials, nil
}

func saveSession(credentials *sts.Credentials, file string) error {
	file = file + ".json"
	json, err := json.Marshal(credentials)
	if err != nil {
		return err
	}

	_, err = os.Stat(path.Dir(file))
	if os.IsNotExist(err) {
		err := os.MkdirAll(path.Dir(file), 0700)
		if err != nil {
			return err
		}
	}
	err = ioutil.WriteFile(file, json, 0600)
	if err != nil {
		return err
	}

	return nil
}

func createEnvFile(credentials *sts.Credentials, file string) error {
	file = file + ".env"
	content := fmt.Sprintf(`# Generated by awsc
export AWS_ACCESS_KEY_ID="%s"
export AWS_SECRET_ACCESS_KEY="%s"
export AWS_SESSION_TOKEN="%s"
export AWS_SECURITY_TOKEN="%s"
`,
		*credentials.AccessKeyId,
		*credentials.SecretAccessKey,
		*credentials.SessionToken,
		*credentials.SessionToken,
	)
	err := ioutil.WriteFile(file, []byte(content), 0600)
	if err != nil {
		return err
	}
	return nil
}

func createScript(
	credentials *sts.Credentials,
	file string,
	profile string,
	cacheDir string,
	sessionName string,
	expiry int64,
) error {
	content := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

AWS_PROFILE='%s' awsc auth \
	--cache-dir '%s' \
	--session-name '%s' \
	--duration-seconds '%d'

. %s.env

eval "$@"
`,
		profile,
		cacheDir,
		sessionName,
		expiry,
		file,
	)
	err := ioutil.WriteFile(file, []byte(content), 0700)
	if err != nil {
		return err
	}
	return nil
}

type ProfileConfig struct {
	SourceProfile string
	MFASerial     string
	RoleARN       string
}

func getProfileConfig(profile string) (*ProfileConfig, error) {
	config := &ProfileConfig{}

	_, err := os.Stat(defaults.SharedConfigFilename())
	if os.IsNotExist(err) {
		return config, nil
	}

	cfg, err := ini.Load(defaults.SharedConfigFilename())
	if err != nil {
		return config, err
	}

	section, _ := cfg.GetSection(profile)
	if section == nil {
		section, _ = cfg.GetSection("profile " + profile)
	}
	if section == nil {
		return config, nil
	}
	config.RoleARN = section.Key("role_arn").String()
	config.MFASerial = section.Key("mfa_serial").String()
	config.SourceProfile = section.Key("source_profile").String()
	return config, nil
}

func createSession(config *aws.Config, profile string, expiry int64) (*sts.Credentials, error) {
	profileConfig, err := getProfileConfig(profile)
	if err != nil {
		return nil, err
	}

	token, err := promptMFAToken()
	if err != nil {
		return nil, err
	}

	sessionProfile := profile
	if profileConfig.SourceProfile != "" {
		sessionProfile = profileConfig.SourceProfile
	}

	sess := session.Must(session.NewSessionWithOptions(session.Options{
		Config:  *config,
		Profile: sessionProfile,
	}))
	service := sts.New(sess)

	var credentials *sts.Credentials

	if profileConfig.RoleARN != "" {
		if expiry > 3600 {
			expiry = 3600
		}
		output, err := service.AssumeRole(&sts.AssumeRoleInput{
			RoleArn:         aws.String(profileConfig.RoleARN),
			SerialNumber:    aws.String(profileConfig.MFASerial),
			TokenCode:       aws.String(strings.TrimSpace(token)),
			DurationSeconds: aws.Int64(expiry),
			RoleSessionName: aws.String(profile),
		})
		if err != nil {
			return nil, err
		}
		credentials = output.Credentials
	} else {
		identity, err := service.GetCallerIdentity(&sts.GetCallerIdentityInput{})
		if err != nil {
			return nil, err
		}
		serialNumber := strings.Replace(*identity.Arn, ":user", ":mfa", 1)

		output, err := service.GetSessionToken(&sts.GetSessionTokenInput{
			SerialNumber:    aws.String(serialNumber),
			TokenCode:       aws.String(strings.TrimSpace(token)),
			DurationSeconds: aws.Int64(expiry),
		})
		if err != nil {
			return nil, err
		}
		credentials = output.Credentials
	}

	return credentials, nil
}

// MFAAuth creates a session with MFA authentication
func MFAAuth(config *aws.Config, out io.Writer, cacheDir string, sessionName string, expiry int64) error {
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "default"
	}

	if sessionName == "" {
		sessionName = profile
	}
	sessionFile := path.Join(cacheDir, sessionName)

	credentials, err := loadSession(sessionFile)
	if err != nil {
		return err
	}

	if credentials == nil {
		credentials, err = createSession(config, profile, expiry)
		if err != nil {
			return err
		}

		err = saveSession(credentials, sessionFile)
		if err != nil {
			return err
		}

		err = createEnvFile(credentials, sessionFile)
		if err != nil {
			return err
		}

		err = createScript(credentials, sessionFile, profile, cacheDir, sessionName, expiry)
		if err != nil {
			return err
		}
	}

	return nil
}
