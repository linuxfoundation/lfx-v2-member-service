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
	dsl.Description("Membership management service - read-only endpoints for member and membership data")

	// List members with search/filtering
	dsl.Method("list-members", func() {
		dsl.Description("List members with pagination, filtering, and search")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			PageSizeAttribute()
			OffsetAttribute()
			FilterAttribute()
			SearchAttribute()
		})

		dsl.Result(func() {
			dsl.Attribute("members", dsl.ArrayOf(MemberResponse), "List of members")
			dsl.Attribute("metadata", ListMetadata, "Pagination metadata")
			dsl.Required("members", "metadata")
		})

		dsl.Error("BadRequest", BadRequestError, "Bad request")
		dsl.Error("InternalServerError", InternalServerError, "Internal server error")
		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")

		dsl.HTTP(func() {
			dsl.GET("/members")
			dsl.Param("version:v")
			dsl.Param("pageSize")
			dsl.Param("offset")
			dsl.Param("filter")
			dsl.Param("search")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusOK)
			dsl.Response("BadRequest", dsl.StatusBadRequest)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// Get a specific membership under a member
	dsl.Method("get-member-membership", func() {
		dsl.Description("Get a specific membership for a member")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			MemberIDAttribute()
			MembershipIDAttribute()
		})

		dsl.Result(func() {
			dsl.Attribute("membership", MembershipResponse, "Membership details")
			ETagAttribute()
			dsl.Required("membership")
		})

		dsl.Error("NotFound", NotFoundError, "Resource not found")
		dsl.Error("InternalServerError", InternalServerError, "Internal server error")
		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")

		dsl.HTTP(func() {
			dsl.GET("/members/{member_id}/memberships/{id}")
			dsl.Param("version:v")
			dsl.Param("member_id")
			dsl.Param("id")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusOK, func() {
				dsl.Body("membership")
				dsl.Header("etag:ETag")
			})
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// List key contacts for a membership under a member
	dsl.Method("list-member-membership-key-contacts", func() {
		dsl.Description("Get key contacts for a specific membership under a member")

		dsl.Security(JWTAuth)

		dsl.Payload(func() {
			BearerTokenAttribute()
			VersionAttribute()
			MemberIDAttribute()
			MembershipIDAttribute()
		})

		dsl.Result(func() {
			dsl.Attribute("contacts", dsl.ArrayOf(KeyContactResponse), "List of key contacts")
			dsl.Required("contacts")
		})

		dsl.Error("NotFound", NotFoundError, "Membership not found")
		dsl.Error("InternalServerError", InternalServerError, "Internal server error")
		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")

		dsl.HTTP(func() {
			dsl.GET("/members/{member_id}/memberships/{id}/key_contacts")
			dsl.Param("version:v")
			dsl.Param("member_id")
			dsl.Param("id")
			dsl.Header("bearer_token:Authorization")
			dsl.Response(dsl.StatusOK)
			dsl.Response("NotFound", dsl.StatusNotFound)
			dsl.Response("InternalServerError", dsl.StatusInternalServerError)
			dsl.Response("ServiceUnavailable", dsl.StatusServiceUnavailable)
		})
	})

	// Health check endpoints
	dsl.Method("readyz", func() {
		dsl.Description("Check if the service is able to take inbound requests.")
		dsl.Meta("swagger:generate", "false")
		dsl.Result(dsl.Bytes, func() {
			dsl.Example("OK")
		})

		dsl.Error("ServiceUnavailable", ServiceUnavailableError, "Service unavailable")

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

	// Serve OpenAPI spec files
	dsl.Files("/_memberships/openapi.json", "gen/http/openapi.json", func() {
		dsl.Meta("swagger:generate", "false")
	})
	dsl.Files("/_memberships/openapi.yaml", "gen/http/openapi.yaml", func() {
		dsl.Meta("swagger:generate", "false")
	})
	dsl.Files("/_memberships/openapi3.json", "gen/http/openapi3.json", func() {
		dsl.Meta("swagger:generate", "false")
	})
	dsl.Files("/_memberships/openapi3.yaml", "gen/http/openapi3.yaml", func() {
		dsl.Meta("swagger:generate", "false")
	})
})
