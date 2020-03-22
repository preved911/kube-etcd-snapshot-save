package main

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	// "path"
	"encoding/hex"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.uber.org/zap"

	"go.etcd.io/etcd/clientv3"
	"go.etcd.io/etcd/clientv3/snapshot"
	"go.etcd.io/etcd/etcdctl/ctlv3/command"
	"go.etcd.io/etcd/pkg/transport"

	"github.com/minio/minio-go/v6"
)

type secureCfg struct {
	cert       string
	key        string
	cacert     string
	serverName string

	insecureTransport  bool
	insecureSkipVerify bool
}

type authCfg struct {
	username string
	password string
}

type s3Cfg struct {
	endpoint  string
	accessKey string
	secretKey string
	bucket    string
}

const (
	defaultDialTimeout      = 2 * time.Second
	defaultCommandTimeOut   = 5 * time.Second
	defaultKeepAliveTime    = 2 * time.Second
	defaultKeepAliveTimeOut = 6 * time.Second

	s3Location = "us-east-1"
)

var (
	globalFlags = command.GlobalFlags{}
	s3Flags     = s3Cfg{}
)

func main() {
	var rootCmd = &cobra.Command{
		Use:   "etcd-snapshot-save",
		Short: "Save and upload etcd snapshot.",
		Long:  "Save and upload etcd snapshot to s3 storage.",
		RunE:  save,
	}

	rootCmd.PersistentFlags().StringSliceVar(&globalFlags.Endpoints, "endpoints", []string{"127.0.0.1:2379"}, "gRPC endpoints")

	rootCmd.PersistentFlags().DurationVar(&globalFlags.DialTimeout, "dial-timeout", defaultDialTimeout, "dial timeout for client connections")
	rootCmd.PersistentFlags().DurationVar(&globalFlags.CommandTimeOut, "command-timeout", defaultCommandTimeOut, "timeout for short running command (excluding dial timeout)")
	rootCmd.PersistentFlags().DurationVar(&globalFlags.KeepAliveTime, "keepalive-time", defaultKeepAliveTime, "keepalive time for client connections")
	rootCmd.PersistentFlags().DurationVar(&globalFlags.KeepAliveTimeout, "keepalive-timeout", defaultKeepAliveTimeOut, "keepalive timeout for client connections")

	// TODO: secure by default when etcd enables secure gRPC by default.
	rootCmd.PersistentFlags().BoolVar(&globalFlags.Insecure, "insecure-transport", true, "disable transport security for client connections")
	// rootCmd.PersistentFlags().BoolVar(&globalFlags.InsecureDiscovery, "insecure-discovery", true, "accept insecure SRV records describing cluster endpoints")
	rootCmd.PersistentFlags().BoolVar(&globalFlags.InsecureSkipVerify, "insecure-skip-tls-verify", false, "skip server certificate verification")
	rootCmd.PersistentFlags().StringVar(&globalFlags.TLS.CertFile, "cert", "", "identify secure client using this TLS certificate file")
	rootCmd.PersistentFlags().StringVar(&globalFlags.TLS.KeyFile, "key", "", "identify secure client using this TLS key file")
	rootCmd.PersistentFlags().StringVar(&globalFlags.TLS.TrustedCAFile, "cacert", "", "verify certificates of TLS-enabled secure servers using this CA bundle")
	rootCmd.PersistentFlags().StringVar(&globalFlags.User, "user", "", "username[:password] for authentication (prompt if password is not supplied)")
	rootCmd.PersistentFlags().StringVar(&globalFlags.Password, "password", "", "password for authentication (if this option is used, --user option shouldn't include password)")
	// rootCmd.PersistentFlags().StringVarP(&globalFlags.TLS.ServerName, "discovery-srv", "d", "", "domain name to query for SRV records describing cluster endpoints")
	// rootCmd.PersistentFlags().StringVarP(&globalFlags.DNSClusterServiceName, "discovery-srv-name", "", "", "service name to query when using DNS discovery")

	// s3 upload flags
	rootCmd.PersistentFlags().StringVarP(&s3Flags.endpoint, "s3-endpoint", "", "", "s3 endpoint")
	rootCmd.PersistentFlags().StringVarP(&s3Flags.accessKey, "s3-access-key", "", "", "s3 access key id")
	rootCmd.PersistentFlags().StringVarP(&s3Flags.secretKey, "s3-secret-key", "", "", "s3 secret access key")
	rootCmd.PersistentFlags().StringVarP(&s3Flags.bucket, "s3-bucket", "", "", "s3 bucket")

	rootCmd.Execute()
}

func save(cmd *cobra.Command, args []string) error {
	path, err := snapshotSave()
	if err != nil {
		return err
	}

	if err := snapshotUpload(path); err != nil {
		return err
	}

	return nil
}

// calculate snapshot md5sum and upload file
func snapshotUpload(snapshotPath string) error {
	snapshotFile, err := os.Open(snapshotPath)
	if err != nil {
		return err
	}

	defer snapshotFile.Close()

	hash := md5.New()

	_, err = io.Copy(hash, snapshotFile)
	if err != nil {
		return err
	}

	md5sumPath := strings.TrimSuffix(snapshotPath, filepath.Ext(".snapshot")) + ".md5"

	md5sumFile, err := os.Create(md5sumPath)
	if err != nil {
		return err
	}

	defer md5sumFile.Close()

	hashString := hex.EncodeToString(hash.Sum(nil))
	md5sumString := fmt.Sprintf("%s  %s\n", hashString, filepath.Base(snapshotPath))

	if _, err := md5sumFile.WriteString(md5sumString); err != nil {
		return err
	}

	return s3upload([]string{snapshotPath, md5sumPath})
}

// upload files from given array to remote s3 storage
func s3upload(paths []string) error {
	minioClient, err := minio.New(s3Flags.endpoint, s3Flags.accessKey, s3Flags.secretKey, true)
	if err != nil {
		log.Fatalln(err)
	}

	// minioClient.MakeBucket(s3Flags.bucket, s3Location)
	err = minioClient.MakeBucket(s3Flags.bucket, s3Location)
	if err != nil {
		exists, errBucketExists := minioClient.BucketExists(s3Flags.bucket)
		if !(errBucketExists == nil && exists) {
			return err
		}
	}

	now := time.Now()

	for _, path := range paths {
		objectName := filepath.Join(
			fmt.Sprintf("%d/%02d", now.Year(), int(now.Month())),
			filepath.Base(path))

		n, err := minioClient.FPutObject(s3Flags.bucket, objectName, path, minio.PutObjectOptions{})
		if err != nil {
			return err
		}

		fmt.Printf("Successfully uploaded %s of size %d\n", objectName, n)
	}

	return nil
}

// create etcd snapshot file
func snapshotSave() (string, error) {
	tlsFlags := globalFlags.TLS

	scfg := &secureCfg{
		cert:               tlsFlags.CertFile,
		key:                tlsFlags.KeyFile,
		cacert:             tlsFlags.TrustedCAFile,
		serverName:         tlsFlags.ServerName,
		insecureTransport:  globalFlags.Insecure,
		insecureSkipVerify: globalFlags.InsecureSkipVerify,
	}

	acfg := &authCfg{
		username: globalFlags.User,
		password: globalFlags.Password,
	}

	cfg, err := newClientCfg(
		globalFlags.Endpoints,
		globalFlags.DialTimeout,
		globalFlags.KeepAliveTime,
		globalFlags.KeepAliveTimeout,
		scfg, acfg)
	if err != nil {
		return "", err
	}

	lg, err := zap.NewProduction()
	if err != nil {
		return "", err
	}

	sp := snapshot.NewV3(lg)

	// ctx, cancel := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 3600*time.Second)
	defer cancel()

	now := time.Now()
	path := fmt.Sprintf("etcd_%d%02d%02d%02d%02d.snapshot",
		now.Year(), int(now.Month()), now.Day(),
		now.Hour(), now.Minute(),
	)
	fmt.Printf("Snapshot file name: %s\n", path)

	if err := sp.Save(ctx, *cfg, path); err != nil {
		return "", err
	}

	return path, nil
}

// generate etcd cfg from given params
func newClientCfg(endpoints []string, dialTimeout, keepAliveTime, keepAliveTimeout time.Duration, scfg *secureCfg, acfg *authCfg) (*clientv3.Config, error) {
	// set tls if any one tls option set
	var cfgtls *transport.TLSInfo
	tlsinfo := transport.TLSInfo{}
	tlsinfo.Logger, _ = zap.NewProduction()
	if scfg.cert != "" {
		tlsinfo.CertFile = scfg.cert
		cfgtls = &tlsinfo
	}

	if scfg.key != "" {
		tlsinfo.KeyFile = scfg.key
		cfgtls = &tlsinfo
	}

	if scfg.cacert != "" {
		tlsinfo.TrustedCAFile = scfg.cacert
		cfgtls = &tlsinfo
	}

	if scfg.serverName != "" {
		tlsinfo.ServerName = scfg.serverName
		cfgtls = &tlsinfo
	}

	cfg := &clientv3.Config{
		Endpoints:            endpoints,
		DialTimeout:          dialTimeout,
		DialKeepAliveTime:    keepAliveTime,
		DialKeepAliveTimeout: keepAliveTimeout,
	}

	if cfgtls != nil {
		clientTLS, err := cfgtls.ClientConfig()
		if err != nil {
			return nil, err
		}
		cfg.TLS = clientTLS
	}

	// if key/cert is not given but user wants secure connection, we
	// should still setup an empty tls configuration for gRPC to setup
	// secure connection.
	if cfg.TLS == nil && !scfg.insecureTransport {
		cfg.TLS = &tls.Config{}
	}

	// If the user wants to skip TLS verification then we should set
	// the InsecureSkipVerify flag in tls configuration.
	if scfg.insecureSkipVerify && cfg.TLS != nil {
		cfg.TLS.InsecureSkipVerify = true
	}

	if acfg != nil {
		cfg.Username = acfg.username
		cfg.Password = acfg.password
	}

	return cfg, nil
}
