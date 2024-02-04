/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package tcc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/hyperledger-labs/fabric-smart-client/platform/view/services/flogging"
	"github.com/hyperledger-labs/fabric-token-sdk/token"
	"github.com/hyperledger-labs/fabric-token-sdk/token/services/vault/translator"
	token2 "github.com/hyperledger-labs/fabric-token-sdk/token/token"
	"github.com/hyperledger/fabric-chaincode-go/shim"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	"github.com/pkg/errors"
)

var logger = flogging.MustGetLogger("token-sdk.tcc")

const (
	InvokeFunction            = "invoke"
	QueryPublicParamsFunction = "queryPublicParams"
	AddCertifierFunction      = "addCertifier"
	QueryTokensFunctions      = "queryTokens"
	AreTokensSpent            = "areTokensSpent"

	PublicParamsPathVarEnv = "PUBLIC_PARAMS_FILE_PATH"
)

type Agent interface {
	EmitKey(val float32, event ...string)
}

type SetupAction struct {
	SetupParameters []byte
}

func (a *SetupAction) GetSetupParameters() ([]byte, error) {
	return a.SetupParameters, nil
}

//go:generate counterfeiter -o mock/validator.go -fake-name Validator . Validator

type Validator interface {
	UnmarshallAndVerify(ledger token.Ledger, binding string, raw []byte) ([]interface{}, error)
}

//go:generate counterfeiter -o mock/public_parameters_manager.go -fake-name PublicParametersManager . PublicParametersManager

type PublicParameters interface {
	GraphHiding() bool
}

type TokenChaincode struct {
	initOnce         sync.Once
	LogLevel         string
	Validator        Validator
	PublicParameters PublicParameters

	PPDigest             []byte
	TokenServicesFactory func([]byte) (PublicParameters, Validator, error)

	MetricsEnabled bool
	MetricsServer  string
}

func (cc *TokenChaincode) Init(stub shim.ChaincodeStubInterface) pb.Response {
	logger.Infof("init token chaincode...")

	ppRaw, err := cc.Params(Params)
	if err != nil {
		return shim.Error(fmt.Sprintf("failed to get public parameters: %s", err))
	}

	w := translator.New("", &rwsWrapper{stub: stub}, "")
	if err := w.Write(&SetupAction{SetupParameters: ppRaw}); err != nil {
		return shim.Error(err.Error())
	}

	return shim.Success(nil)
}

func (cc *TokenChaincode) Invoke(stub shim.ChaincodeStubInterface) (res pb.Response) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("invoke triggered panic: %s\n%s\n", r, debug.Stack())
			res = shim.Error(fmt.Sprintf("failed responding [%s]", r))
		} else {
			logger.Infof("execution terminated with status [%d]", res.Status)
		}
	}()

	args := stub.GetArgs()

	argStrs := make([]string, len(args))
	for i, arg := range args {
		argStrs[i] = string(arg)
	}

	logger.Infof("[TokenChaincode.Invoke] get args %s", strings.Join(argStrs, ", "))

	switch l := len(args); l {
	case 0:
		return shim.Error("missing parameters")
	default:
		logger.Infof("running function [%s]", string(args[0]))
		switch f := string(args[0]); f {
		case InvokeFunction:
			logger.Infof("running args: invoke")

			if len(args) != 1 {
				return shim.Error("empty token request")
			}
			// extract token request from transient
			t, err := stub.GetTransient()
			if err != nil {
				return shim.Error("failed getting transient")
			}
			tokenRequest, ok := t["token_request"]
			if !ok {
				return shim.Error("failed getting token request, entry not found")
			}

			logger.Infof("[in case invoice] token request: %s", string(tokenRequest))

			return cc.ProcessRequest(tokenRequest, stub)
		case QueryPublicParamsFunction:
			logger.Infof("running args: queryPublicParams")
			return cc.QueryPublicParams(stub)
		case QueryTokensFunctions:
			logger.Infof("running args: queryTokens")

			if len(args) != 2 {
				return shim.Error("request to retrieve tokens is empty")
			}
			return cc.QueryTokens(args[1], stub)
		case AreTokensSpent:
			logger.Infof("running args: areTokensSpent")
			if len(args) != 2 {
				return shim.Error("request to check if tokens are spent is empty")
			}
			return cc.AreTokensSpent(args[1], stub)
		default:
			return shim.Error(fmt.Sprintf("function not [%s] recognized", f))
		}
	}
}

// -------------- func helper ---------------
func (cc *TokenChaincode) Params(builtInParams string) ([]byte, error) {
	params := cc.ReadParamsFromFile()
	if params == "" {
		if len(builtInParams) == 0 {
			return nil, errors.New("no params provided")
		} else {
			params = builtInParams
		}
	}

	ppRaw, err := base64.StdEncoding.DecodeString(params)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed decoding params [%s]", params)
	}

	return ppRaw, nil
}

func (cc *TokenChaincode) GetValidator(builtInParams string) (Validator, error) {
	logger.Infof("call func GetValidator...")

	var firstInitError error
	cc.initOnce.Do(func() {
		if err := cc.Initialize(builtInParams); err != nil {
			firstInitError = err
		}
	})

	if firstInitError != nil {
		return nil, firstInitError
	}
	return cc.Validator, nil
}

func (cc *TokenChaincode) Initialize(builtInParams string) error {
	logger.Infof("call func initialize...")
	logger.Infof("reading public parameters...")
	logger.Infof("Initialize func param builtInParams: %s", builtInParams)

	ppRaw, err := cc.Params(builtInParams)
	if err != nil {
		return errors.WithMessagef(err, "failed reading public parameters")
	}

	logger.Infof("[TokenChaincode.Initialize] ppRaw in zkatlog_pp.json")

	logger.Infof("instantiate public parameter manager and validator...")
	ppm, validator, err := cc.TokenServicesFactory(ppRaw)
	logger.Infof("instantiate public parameter manager and validator done with err [%v]", err)
	if err != nil {
		return errors.Wrap(err, "failed to instantiate public parameter manager and validator")
	}
	cc.PublicParameters = ppm
	cc.Validator = validator

	return nil
}

func (cc *TokenChaincode) ReadParamsFromFile() string {
	publicParamsPath := os.Getenv(PublicParamsPathVarEnv)
	if publicParamsPath == "" {
		fmt.Println("no PUBLIC_PARAMS_FILE_PATH provided")
		return ""
	}

	fmt.Println("reading " + publicParamsPath + " ...")
	paramsAsBytes, err := os.ReadFile(publicParamsPath)
	if err != nil {
		fmt.Printf(
			"unable to read file %s (%s). continue looking pub params from init args or cc\n", publicParamsPath, err.Error(),
		)
		return ""
	}

	return base64.StdEncoding.EncodeToString(paramsAsBytes)
}

func (cc *TokenChaincode) ProcessRequest(raw []byte, stub shim.ChaincodeStubInterface) pb.Response {
	logger.Infof("call func ProcessRequest...")
	logger.Infof("raw data: %s", string(raw))
	validator, err := cc.GetValidator(Params)
	if err != nil {
		return shim.Error(err.Error())
	}

	// Verify
	actions, err := validator.UnmarshallAndVerify(stub, stub.GetTxID(), raw)
	if err != nil {
		return shim.Error("failed to verify token request: " + err.Error())
	}

	// Write

	w := translator.New(stub.GetTxID(), &rwsWrapper{stub: stub}, "")
	for _, action := range actions {
		err = w.Write(action)
		if err != nil {
			return shim.Error("failed to write token action: " + err.Error())
		}
	}
	err = w.CommitTokenRequest(raw, false)
	if err != nil {
		return shim.Error("failed to write token request:" + err.Error())
	}

	return shim.Success(nil)
}

func (cc *TokenChaincode) QueryPublicParams(stub shim.ChaincodeStubInterface) pb.Response {
	w := translator.New(stub.GetTxID(), &rwsWrapper{stub: stub}, "")
	raw, err := w.ReadSetupParameters()
	if err != nil {
		return shim.Error("failed to retrieve public parameters: " + err.Error())
	}
	if len(raw) == 0 {
		return shim.Error("need to initialize public parameters")
	}

	logger.Infof("query public params, size[%d]", len(raw))

	return shim.Success(raw)
}

func (cc *TokenChaincode) QueryTokens(idsRaw []byte, stub shim.ChaincodeStubInterface) pb.Response {
	var ids []*token2.ID
	if err := json.Unmarshal(idsRaw, &ids); err != nil {
		logger.Errorf("failed unmarshalling tokens ids: [%s]", err)
		return shim.Error(err.Error())
	}

	logger.Infof("query tokens [%v]...", ids)

	w := translator.New(stub.GetTxID(), &rwsWrapper{stub: stub}, "")
	res, err := w.QueryTokens(ids)
	if err != nil {
		logger.Errorf("failed query tokens [%v]: [%s]", ids, err)
		return shim.Error(fmt.Sprintf("failed query tokens [%v]: [%s]", ids, err))
	}
	raw, err := json.Marshal(res)
	if err != nil {
		logger.Errorf("failed marshalling tokens: [%s]", err)
		return shim.Error(fmt.Sprintf("failed marshalling tokens: [%s]", err))
	}
	return shim.Success(raw)
}

func (cc *TokenChaincode) AreTokensSpent(idsRaw []byte, stub shim.ChaincodeStubInterface) pb.Response {
	logger.Infof("call func AreTokensSpent...")
	logger.Infof("idsRaw: %s", string(idsRaw))
	_, err := cc.GetValidator(Params)
	if err != nil {
		return shim.Error(err.Error())
	}

	var ids []string
	if err := json.Unmarshal(idsRaw, &ids); err != nil {
		logger.Errorf("failed unmarshalling tokens ids: [%s]", err)
		return shim.Error(err.Error())
	}

	logger.Debugf("check if tokens are spent [%v]...", ids)

	w := translator.New(stub.GetTxID(), &rwsWrapper{stub: stub}, "")
	res, err := w.AreTokensSpent(ids, cc.PublicParameters.GraphHiding())
	if err != nil {
		logger.Errorf("failed to check if tokens are spent [%v]: [%s]", ids, err)
		return shim.Error(fmt.Sprintf("failed to check if tokens are spent [%v]: [%s]", ids, err))
	}
	raw, err := json.Marshal(res)
	if err != nil {
		logger.Errorf("failed marshalling spent flags: [%s]", err)
		return shim.Error(fmt.Sprintf("failed marshalling spent flags: [%s]", err))
	}
	return shim.Success(raw)
}

// -------------- end func helper ---------------
