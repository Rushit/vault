package transit

import (
	"encoding/base64"
	"fmt"

	"github.com/hashicorp/vault/helper/errutil"
	"github.com/hashicorp/vault/helper/jsonutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

// BatchDecryptionItemRequest represents an item in the batch decryption
// request
type BatchDecryptionItemRequest struct {
	// Context for key derivation. This is required for derived keys.
	Context string `json:"context" structs:"context" mapstructure:"context"`

	// Ciphertext for decryption
	Ciphertext string `json:"ciphertext" structs:"ciphertext" mapstructure:"ciphertext"`

	// Nonce to be used when v1 convergent encryption is used
	Nonce string `json:"nonce" structs:"nonce" mapstructure:"nonce"`
}

// BatchDecryptionItemResponse represents an item in the batch decryption
// response
type BatchDecryptionItemResponse struct {
	// Plaintext for the ciphertext present in the corresponsding batch
	// request item
	Plaintext string `json:"plaintext" structs:"plaintext" mapstructure:"plaintext"`

	// Error, if set, represents a failure encountered while decrypting a
	// corresponding batch request item
	Error string `json:"error" structs:"error" mapstructure:"error"`
}

func (b *backend) pathDecrypt() *framework.Path {
	return &framework.Path{
		Pattern: "decrypt/" + framework.GenericNameRegex("name"),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Name of the policy",
			},

			"ciphertext": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `
The ciphertext to decrypt, provided as returned by encrypt.`,
			},

			"context": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `
Base64 encoded context for key derivation. Required if key derivation is
enabled.`,
			},

			"nonce": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `
Base64 encoded nonce value used during encryption. Must be provided if
convergent encryption is enabled for this key and the key was generated with
Vault 0.6.1. Not required for keys created in 0.6.2+.`,
			},

			"batch": &framework.FieldSchema{
				Type: framework.TypeString,
				Description: `
Base64 encoded list of items to be decrypted in a single batch. When this
parameter is set, if the parameters 'ciphertext', 'context' and 'nonce' are
also set, they will be ignored. JSON format for the input goes like this:
[
  {
    "context": "context1",
    "ciphertext": "vault:v1:/DupSiSbX/ATkGmKAmhqD0tvukByrx6gmps7dVI="
  },
  {
    "context": "context2",
    "ciphertext": "vault:v1:XjsPWPjqPrBi1N2Ms2s1QM798YyFWnO4TR4lsFA="
  },
  ...
]`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathDecryptWrite,
		},

		HelpSynopsis:    pathDecryptHelpSyn,
		HelpDescription: pathDecryptHelpDesc,
	}
}

func (b *backend) pathDecryptWrite(
	req *logical.Request, d *framework.FieldData) (*logical.Response, error) {

	// Get the policy
	p, lock, err := b.lm.GetPolicyShared(req.Storage, d.Get("name").(string))
	if lock != nil {
		defer lock.RUnlock()
	}
	if err != nil {
		return nil, err
	}
	if p == nil {
		return logical.ErrorResponse("policy not found"), logical.ErrInvalidRequest
	}

	batchInputRaw := d.Get("batch").(string)
	var batchInputArray []BatchDecryptionItemRequest
	var batchInput []byte
	if len(batchInputRaw) != 0 {
		batchInput, err = base64.StdEncoding.DecodeString(batchInputRaw)
		if err != nil {
			return logical.ErrorResponse("failed to base64-decode batch input"), logical.ErrInvalidRequest
		}

		if err := jsonutil.DecodeJSON([]byte(batchInput), &batchInputArray); err != nil {
			return nil, fmt.Errorf("invalid input: %v", err)
		}

	} else {
		ciphertext := d.Get("ciphertext").(string)
		if len(ciphertext) == 0 {
			return logical.ErrorResponse("missing ciphertext to decrypt"), logical.ErrInvalidRequest
		}

		batchInputArray = make([]BatchDecryptionItemRequest, 1)
		batchInputArray[0] = BatchDecryptionItemRequest{
			Ciphertext: ciphertext,
			Context:    d.Get("context").(string),
			Nonce:      d.Get("nonce").(string),
		}
	}

	if len(batchInputArray) == 0 {
		return logical.ErrorResponse("missing input to process"), logical.ErrInvalidRequest
	}

	var contextSet bool
	switch len(batchInputArray) {
	case 1:
		contextSet = batchInputArray[0].Context != ""
	default:
		contextSet = true
		for _, item := range batchInputArray {
			if item.Context == "" && contextSet {
				contextSet = false
			}

			if item.Context != "" && !contextSet {
				return logical.ErrorResponse("context should be set either in all the request blocks or in none"), logical.ErrInvalidRequest
			}
		}
	}

	batchResponseItems := make([]BatchDecryptionItemResponse, len(batchInputArray))
	for i, item := range batchInputArray {
		if item.Ciphertext == "" {
			batchResponseItems[i] = BatchDecryptionItemResponse{
				Error: "missing ciphertext to decrypt",
			}
			continue
		}

		var itemContext []byte
		if len(item.Context) != 0 {
			itemContext, err = base64.StdEncoding.DecodeString(item.Context)
			if err != nil {
				batchResponseItems[i] = BatchDecryptionItemResponse{
					Error: "failed to base64-decode context",
				}
				continue
			}
		}

		var itemNonce []byte
		if len(item.Nonce) != 0 {
			itemNonce, err = base64.StdEncoding.DecodeString(item.Nonce)
			if err != nil {
				batchResponseItems[i] = BatchDecryptionItemResponse{
					Error: "failed to base64-decode nonce",
				}
			}
			continue
		}

		plaintext, err := p.Decrypt(itemContext, itemNonce, item.Ciphertext)
		if err != nil {
			switch err.(type) {
			case errutil.UserError:
				return logical.ErrorResponse(err.Error()), logical.ErrInvalidRequest
			case errutil.InternalError:
				return nil, err
			default:
				return nil, err
			}
		}

		batchResponseItems[i] = BatchDecryptionItemResponse{
			Plaintext: plaintext,
		}
	}

	resp := &logical.Response{}
	if len(batchInputRaw) != 0 {
		batchResponseJSON, err := jsonutil.EncodeJSON(batchResponseItems)
		if err != nil {
			return nil, fmt.Errorf("failed to JSON encode batch response")
		}
		resp.Data = map[string]interface{}{
			"data": string(batchResponseJSON),
		}
	} else {
		if batchResponseItems[0].Error != "" {
			return nil, fmt.Errorf(batchResponseItems[0].Error)
		}
		resp.Data = map[string]interface{}{
			"plaintext": batchResponseItems[0].Plaintext,
		}
	}

	return resp, nil
}

const pathDecryptHelpSyn = `Decrypt a ciphertext value using a named key`

const pathDecryptHelpDesc = `
This path uses the named key from the request path to decrypt a user
provided ciphertext. The plaintext is returned base64 encoded.
`
