// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package design

import (
	"goa.design/goa/v3/dsl"
)

var _ = dsl.API("membership", func() {
	dsl.Title("Membership Management Service")
})

// JWTAuth is the DSL JWT security type for authentication.
var JWTAuth = dsl.JWTSecurity("jwt", func() {
	dsl.Description("Heimdall authorization")
})

// Service describes the membership service
var _ = dsl.Service("membership-service", func() {
	dsl.Description("Membership management service — direct resource endpoints for B2B orgs, memberships, and key contacts")

	// ── B2B Organizations (Account) ──────────────────────────────────────────

	dsl.Method("get-b2b-org", func() {
		dsl.Description("Get a specific B2B organization by UID")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			IfNoneMatchAttribute()
			IfModifiedSinceAttribute()
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("b2b_org", B2BOrgResponse, "B2B organization details")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("b2b_org")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.GET("/b2b_orgs/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_none_match:If-None-Match")
			dsl.Header("if_modified_since:If-Modified-Since")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("b2b_org")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("create-b2b-org", func() {
		dsl.Description("Create a new B2B organization")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Extend(B2BOrgCreateBody)
		})

		dsl.Result(func() {
			dsl.Attribute("b2b_org", B2BOrgResponse, "Newly created B2B organization")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("b2b_org")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Response(dsl.StatusCreated, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("b2b_org")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("update-b2b-org", func() {
		dsl.Description("Update a B2B organization")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			IfMatchAttribute()
			dsl.Extend(B2BOrgUpdateBody)
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("b2b_org", B2BOrgResponse, "Updated B2B organization")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("b2b_org")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.PUT("/b2b_orgs/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("b2b_org")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("upload-b2b-org-logo", func() {
		dsl.Description("Upload a B2B organization logo (PNG/JPEG/SVG, max 2MB) to object storage and set it as the org's logo URL. " +
			"The request body is the raw logo image bytes -- not a JSON envelope -- sent with Content-Type set to one of " +
			"image/png, image/jpeg, or image/svg+xml (echoed in the content_type header attribute below), and Content-Length " +
			"set to the byte count (echoed in content_length). This isn't reflected as a structured OpenAPI request body " +
			"because this endpoint uses SkipRequestBodyEncodeDecode for direct streaming access, which Goa's generator does " +
			"not support combining with a Body(...) declaration.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			// if_match is mandatory here, unlike the other If-Match-bearing
			// methods in this file: this endpoint writes bytes to a shared
			// object-storage key, so without a real optimistic-concurrency
			// check two concurrent uploads can both call Update successfully
			// and leave the final Salesforce URL and the final object-storage
			// bytes chosen by two independently-raced writes (see the
			// LFXV2-2016 Copilot review on PR #87).
			IfMatchAttribute()
			dsl.Attribute("content_type", dsl.String, "MIME type of the uploaded logo (image/png, image/jpeg, or image/svg+xml)", func() {
				dsl.Example("image/png")
			})
			dsl.Attribute("content_length", dsl.Int64, "Size of the uploaded logo in bytes", func() {
				dsl.Example(102400)
			})
			dsl.Required("uid", "content_type", "if_match")
		})

		dsl.Result(func() {
			dsl.Attribute("b2b_org", B2BOrgResponse, "Updated B2B organization")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("b2b_org")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request (unsupported content type or file too large)")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs/{uid}/logo")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Header("content_type:Content-Type")
			dsl.Header("content_length:Content-Length")
			// Goa forbids Body(...) together with SkipRequestBodyEncodeDecode
			// ("Cannot define a request body when using
			// SkipRequestBodyEncodeDecode") so the raw binary body can't be
			// modeled structurally for OpenAPI here -- it's documented in
			// prose on the method Description above instead (see the
			// LFXV2-2016 Copilot review on PR #87).
			dsl.SkipRequestBodyEncodeDecode()
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("b2b_org")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("get-b2b-org-settings", func() {
		dsl.Description("Get the access-control settings (writers and auditors) for a B2B organization")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("settings", B2BOrgSettingsResponse, "B2B organization access-control settings")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("settings")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.GET("/b2b_orgs/{uid}/settings")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("settings")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("update-b2b-org-settings", func() {
		dsl.Description("Replace the writers and/or auditors list on a B2B organization (full-replace semantics)")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			IfMatchAttribute()
			dsl.Extend(B2BOrgSettingsUpdateBody)
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("settings", B2BOrgSettingsResponse, "Updated B2B organization access-control settings")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("settings")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification — retry with fresh settings")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.PUT("/b2b_orgs/{uid}/settings")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("settings")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("add-b2b-org-settings-user", func() {
		dsl.Description("Add (invite) a single principal to a B2B organization's writers or auditors. Per-principal merge: existing members are preserved; the new entry lands as a pending invite (no username yet).")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			IfMatchAttribute()
			dsl.Extend(OrgUserAddBody)
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("settings", B2BOrgSettingsResponse, "Updated B2B organization access-control settings")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("settings")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Principal already present, or concurrent modification — retry with fresh settings")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs/{uid}/settings/users")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("settings")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("update-b2b-org-settings-user-role", func() {
		dsl.Description("Change a single principal's role (writer⇄auditor) on a B2B organization. Per-principal merge: the principal's username and invite lifecycle are preserved; all other members are untouched.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("email", dsl.String, "Email of the principal to modify", func() {
				dsl.Format(dsl.FormatEmail)
				dsl.Example("alice@example.com")
			})
			IfMatchAttribute()
			dsl.Extend(OrgUserRoleBody)
			dsl.Required("uid", "email")
		})

		dsl.Result(func() {
			dsl.Attribute("settings", B2BOrgSettingsResponse, "Updated B2B organization access-control settings")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("settings")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Organization or principal not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification, or last-Admin invariant — retry with fresh settings")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.PUT("/b2b_orgs/{uid}/settings/users/{email}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("email")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("settings")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("delete-b2b-org-settings-user", func() {
		dsl.Description("Remove a single principal's access (revoke an accepted grant or cancel a pending invite) from a B2B organization. Per-principal merge: all other members are untouched.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("email", dsl.String, "Email of the principal to remove", func() {
				dsl.Format(dsl.FormatEmail)
				dsl.Example("alice@example.com")
			})
			IfMatchAttribute()
			dsl.Required("uid", "email")
		})

		dsl.Result(func() {
			dsl.Attribute("settings", B2BOrgSettingsResponse, "Updated B2B organization access-control settings")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("settings")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Organization or principal not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification, or last-Admin invariant — retry with fresh settings")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.DELETE("/b2b_orgs/{uid}/settings/users/{email}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("email")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("settings")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// ── Project Memberships (Asset) ──────────────────────────────────────────

	dsl.Method("get-project-membership", func() {
		dsl.Description("Get a specific project membership by UID")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "Project membership UID", func() {
				dsl.Example("02i2M000009ABCdIAM")
			})
			IfNoneMatchAttribute()
			IfModifiedSinceAttribute()
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("project_membership", ProjectMembershipResponse, "Project membership details")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("project_membership")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.GET("/project_memberships/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_none_match:If-None-Match")
			dsl.Header("if_modified_since:If-Modified-Since")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("project_membership")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// ── Key Contacts (Project_Role__c) ───────────────────────────────────────

	dsl.Method("get-key-contact", func() {
		dsl.Description("Get a specific key contact by UID")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("membership_uid", dsl.String, "Parent membership UID", func() {
				dsl.Example("02i2M000009ABCdIAM")
			})
			dsl.Attribute("uid", dsl.String, "Key contact UID", func() {
				dsl.Example("a0K2M000000ABCdUAG")
			})
			IfNoneMatchAttribute()
			IfModifiedSinceAttribute()
			dsl.Required("membership_uid", "uid")
		})

		dsl.Result(func() {
			dsl.Attribute("key_contact", ProjectKeyContactResponse, "Key contact details")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("key_contact")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.GET("/project_memberships/{membership_uid}/key_contacts/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("membership_uid")
			dsl.Param("uid")
			dsl.Header("if_none_match:If-None-Match")
			dsl.Header("if_modified_since:If-Modified-Since")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("key_contact")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("create-key-contact", func() {
		dsl.Description("Create a new key contact")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("membership_uid", dsl.String, "Parent membership UID", func() {
				dsl.Example("02i2M000009ABCdIAM")
			})
			dsl.Extend(KeyContactCreateBody)
			dsl.Required("membership_uid")
		})

		dsl.Result(func() {
			dsl.Attribute("key_contact", ProjectKeyContactResponse, "Newly created key contact")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("key_contact")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Capacity limit or duplicate key contact")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/project_memberships/{membership_uid}/key_contacts")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("membership_uid")
			dsl.Response(dsl.StatusCreated, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("key_contact")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("update-key-contact", func() {
		dsl.Description("Update a key contact")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("membership_uid", dsl.String, "Parent membership UID", func() {
				dsl.Example("02i2M000009ABCdIAM")
			})
			dsl.Attribute("uid", dsl.String, "Key contact UID", func() {
				dsl.Example("a0K2M000000ABCdUAG")
			})
			IfMatchAttribute()
			dsl.Extend(KeyContactUpdateBody)
			dsl.Required("membership_uid", "uid")
		})

		dsl.Result(func() {
			dsl.Attribute("key_contact", ProjectKeyContactResponse, "Updated key contact")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("key_contact")
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Capacity limit or duplicate key contact")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.PUT("/project_memberships/{membership_uid}/key_contacts/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("membership_uid")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
				dsl.Body("key_contact")
			})
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("delete-key-contact", func() {
		dsl.Description("Delete a key contact")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("membership_uid", dsl.String, "Parent membership UID", func() {
				dsl.Example("02i2M000009ABCdIAM")
			})
			dsl.Attribute("uid", dsl.String, "Key contact UID", func() {
				dsl.Example("a0K2M000000ABCdUAG")
			})
			IfMatchAttribute()
			dsl.Required("membership_uid", "uid")
		})

		dsl.Result(dsl.Empty)

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.DELETE("/project_memberships/{membership_uid}/key_contacts/{uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("membership_uid")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusNoContent)
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// ── Admin Actions ────────────────────────────────────────────────────────

	dsl.Method("admin-reindex", func() {
		dsl.Description("Trigger a reindex of cached entities. " +
			"Operational note: key_contact is high-volume (~300k records in prod); " +
			"reindex only the active window by passing a `since` ~2 years back " +
			"(e.g. since=2024-06-01T00:00:00Z) rather than a full key_contact reindex.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Extend(AdminReindexPayload)
		})

		dsl.Result(func() {
			dsl.Extend(AdminReindexResult)
		})

		dsl.Error("NotImplemented", dsl.ErrorResult, "Endpoint not implemented")
		dsl.Error("NotFound", dsl.ErrorResult, "Resource not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/admin/reindex")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Response(dsl.StatusAccepted)
			dsl.Response("NotImplemented", dsl.StatusNotImplemented)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// ── Health checks ────────────────────────────────────────────────────────

	dsl.Method("readyz", func() {
		dsl.Description("Check if the service is able to take inbound requests.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() {
			dsl.Example("OK")
		})

		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.GET("/readyz")
			dsl.Response(dsl.StatusOK, func() {
				dsl.ContentType("text/plain")
			})
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("livez", func() {
		dsl.Description("Check if the service is alive.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() {
			dsl.Example("OK")
		})
		dsl.HTTP(func() {
			dsl.GET("/livez")
			dsl.Response(dsl.StatusOK, func() {
				dsl.ContentType("text/plain")
			})
		})
	})

	dsl.Method("debug-vars", func() {
		dsl.Description("Expose expvar debug variables as JSON. Accessible via kubectl port-forward; not exposed by ingress.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes)
		dsl.HTTP(func() {
			dsl.GET("/debug/vars")
			dsl.Response(dsl.StatusOK, func() {
				// text/plain is intentional: when the result type is Bytes and
				// the content type is application/json, the Goa response encoder
				// treats the []byte value as a JSON value to encode, which
				// base64-encodes the payload. text/plain causes the Goa
				// textEncoder to write the bytes directly to the response
				// writer. The DebugVars implementation builds valid JSON itself
				// via expvar.Do, so the wire format is correct JSON regardless
				// of the declared content type.
				dsl.ContentType("text/plain")
			})
		})
	})

	// ── OpenAPI spec files ────────────────────────────────────────────────────

	dsl.Files("/_memberships/openapi.json", "gen/http/openapi.json", func() {
		dsl.Meta("swagger:generate", "false")
	})
	dsl.Files("/_memberships/openapi.yaml", "gen/http/openapi.yaml", func() {
		dsl.Meta("swagger:generate", "false")
	})
	dsl.Files("/_memberships/openapi3.json", "gen/http/openapi3.json", func() {
		dsl.Meta("swagger:generate", "false")
	})
	// ── Workspaces ────────────────────────────────────────────────────────────

	dsl.Method("create-b2b-org-workspace", func() {
		dsl.Description("Create a new workspace within a b2b_org. Name must be unique within the org.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			IfMatchAttribute()
			dsl.Extend(WorkspaceCreateBody)
			dsl.Required("uid")
		})

		dsl.Result(func() {
			dsl.Attribute("workspace", WorkspaceResponse, "The created workspace")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("workspace")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Organization not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Workspace name already exists, or concurrent modification — retry")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs/{uid}/workspaces")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusCreated, func() {
				dsl.Body("workspace")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("update-b2b-org-workspace", func() {
		dsl.Description("Rename an existing workspace. Name must be unique within the org.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("workspace_uid", dsl.String, "Workspace UID", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
			})
			IfMatchAttribute()
			dsl.Extend(WorkspaceUpdateBody)
			dsl.Required("uid", "workspace_uid")
		})

		dsl.Result(func() {
			dsl.Attribute("workspace", WorkspaceResponse, "The updated workspace")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("workspace")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Workspace not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Workspace name already exists, or concurrent modification — retry")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.PUT("/b2b_orgs/{uid}/workspaces/{workspace_uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("workspace_uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("workspace")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("delete-b2b-org-workspace", func() {
		dsl.Description("Delete a workspace and all its project associations (cascade delete).")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("workspace_uid", dsl.String, "Workspace UID", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
			})
			IfMatchAttribute()
			dsl.Required("uid", "workspace_uid")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Workspace not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.DELETE("/b2b_orgs/{uid}/workspaces/{workspace_uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("workspace_uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusNoContent)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("add-b2b-org-workspace-project", func() {
		dsl.Description("Add a single project to a workspace. The caller supplies project_slug (and optional project_name); member-service generates the project_uid. Idempotent on project_slug: re-adding the same slug is a no-op.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("workspace_uid", dsl.String, "Workspace UID", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
			})
			IfMatchAttribute()
			dsl.Extend(WorkspaceProjectAddBody)
			dsl.Required("uid", "workspace_uid")
		})

		dsl.Result(func() {
			dsl.Attribute("workspace", WorkspaceResponse, "The updated workspace")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("workspace")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Workspace not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request (e.g. blank project_slug)")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification — retry")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs/{uid}/workspaces/{workspace_uid}/projects")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("workspace_uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("workspace")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("bulk-add-b2b-org-workspace-projects", func() {
		dsl.Description("Add multiple projects to a workspace in one operation. Each item supplies project_slug (and optional project_name); member-service generates a project_uid per item. Idempotent on project_slug. Partially succeeds: valid items are written; per-item failures (e.g. blank project_slug) are reported in the response.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("workspace_uid", dsl.String, "Workspace UID", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
			})
			IfMatchAttribute()
			dsl.Extend(WorkspaceProjectsBulkAddBody)
			dsl.Required("uid", "workspace_uid")
		})

		dsl.Result(WorkspaceBulkResponse)

		dsl.Error("NotFound", dsl.ErrorResult, "Workspace not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification — retry")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.POST("/b2b_orgs/{uid}/workspaces/{workspace_uid}/projects/bulk")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("workspace_uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Method("remove-b2b-org-workspace-project", func() {
		dsl.Description("Remove a project association from a workspace by its member-service-generated project_uid.")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			dsl.Attribute("uid", dsl.String, "B2B organization UID", func() {
				dsl.Example("001B000000IqhSLIAZ")
			})
			dsl.Attribute("workspace_uid", dsl.String, "Workspace UID", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("4c46585f-9f01-8bda-a0a5-f0c8eeef7fff")
			})
			dsl.Attribute("project_uid", dsl.String, "Association UID to remove — the member-service-generated UUID returned when the project was added (see project_uid in the workspace projects list)", func() {
				dsl.Format(dsl.FormatUUID)
				dsl.Example("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
			})
			IfMatchAttribute()
			dsl.Required("uid", "workspace_uid", "project_uid")
		})

		dsl.Result(func() {
			dsl.Attribute("workspace", WorkspaceResponse, "The updated workspace")
			ETagAttribute()
			LastModifiedAttribute()
			dsl.Required("workspace")
		})

		dsl.Error("NotFound", dsl.ErrorResult, "Workspace or project not found")
		dsl.Error("BadRequest", dsl.ErrorResult, "Bad request")
		dsl.Error("Conflict", dsl.ErrorResult, "Concurrent modification — retry")
		dsl.Error("PreconditionFailed", dsl.ErrorResult, "Precondition failed")
		dsl.Error("InternalServerError", dsl.ErrorResult, "Internal server error", func() { dsl.Fault() })
		dsl.Error("ServiceUnavailable", dsl.ErrorResult, "Service unavailable", func() { dsl.Temporary() })

		dsl.HTTP(func() {
			dsl.DELETE("/b2b_orgs/{uid}/workspaces/{workspace_uid}/projects/{project_uid}")
			dsl.Header("bearer_token:Authorization")
			dsl.Param("version:v")
			dsl.Param("uid")
			dsl.Param("workspace_uid")
			dsl.Param("project_uid")
			dsl.Header("if_match:If-Match")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("workspace")
				dsl.Header("etag:ETag")
				dsl.Header("last_modified:Last-Modified")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("Conflict", dsl.StatusConflict)
			dsl.Response("PreconditionFailed", dsl.StatusPreconditionFailed)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	dsl.Files("/_memberships/openapi3.yaml", "gen/http/openapi3.yaml", func() {
		dsl.Meta("swagger:generate", "false")
	})
})
