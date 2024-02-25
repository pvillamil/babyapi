package babyapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

var respondOnce sync.Once

func defaultResponseCodes() map[string]int {
	return map[string]int{
		http.MethodGet:    http.StatusOK,
		http.MethodDelete: http.StatusNoContent,
		http.MethodPost:   http.StatusCreated,
		http.MethodPatch:  http.StatusOK,
		http.MethodPut:    http.StatusOK,
	}
}

// BuilderError is used for combining errors that may occur when constructing a new API
type BuilderError struct {
	errors []error
}

func (e BuilderError) Error() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("encountered %d errors constructing API:\n", len(e.errors)))

	for _, err := range e.errors {
		sb.WriteString(fmt.Sprintf("- %v\n", err))
	}

	return sb.String()
}

var _ error = BuilderError{}

// HTMLer allows for easily represending reponses as HTML strings when accepted content
// type is text/html
type HTMLer interface {
	HTML(*http.Request) string
}

// Create API routes on the given router
func (a *API[T]) Route(r chi.Router) error {
	a.readOnly.TryLock()

	if len(a.errors) > 0 {
		return BuilderError{a.errors}
	}

	respondOnce.Do(func() {
		render.Respond = func(w http.ResponseWriter, r *http.Request, v interface{}) {
			if render.GetAcceptedContentType(r) == render.ContentTypeHTML {
				htmler, ok := v.(HTMLer)
				if ok {
					render.HTML(w, r, htmler.HTML(r))
					return
				}
			}

			render.DefaultResponder(w, r, v)
		}
	})

	for _, m := range a.middlewares {
		r.Use(m)
	}

	if a.parent == nil {
		a.doCustomRoutes(r, a.rootRoutes)
	}

	var returnErr error
	r.Route(a.base, func(r chi.Router) {
		// Only set these middleware for root-level API
		if a.parent == nil {
			a.DefaultMiddleware(r)
		}

		if a.rootAPI {
			returnErr = a.rootAPIRoutes(r)
			return
		}

		routeIfNotNil(r.With(a.requestBodyMiddleware).Post, "/", a.Post)
		routeIfNotNil(r.Get, "/", a.GetAll)

		r.With(a.resourceExistsMiddleware).Route(fmt.Sprintf("/{%s}", a.IDParamKey()), func(r chi.Router) {
			for _, m := range a.idMiddlewares {
				r.Use(m)
			}

			routeIfNotNil(r.Get, "/", a.Get)
			routeIfNotNil(r.Delete, "/", a.Delete)
			routeIfNotNil(r.With(a.requestBodyMiddleware).Put, "/", a.Put)
			routeIfNotNil(r.With(a.requestBodyMiddleware).Patch, "/", a.Patch)

			for _, subAPI := range a.subAPIs {
				err := subAPI.Route(r)
				if err != nil {
					returnErr = fmt.Errorf("error creating routes for %q: %w", subAPI.Name(), err)
					return
				}
			}

			a.doCustomRoutes(r, a.customIDRoutes)
		})
		if returnErr != nil {
			return
		}

		a.doCustomRoutes(r, a.customRoutes)
	})

	return returnErr
}

// rootAPIRoutes creates different routes for a root API that doesn't deal with any resources
func (a *API[T]) rootAPIRoutes(r chi.Router) error {
	routeIfNotNil(r.Post, "/", a.Post)
	routeIfNotNil(r.Get, "/", a.Get)
	routeIfNotNil(r.Delete, "/", a.Delete)
	routeIfNotNil(r.Put, "/", a.Put)
	routeIfNotNil(r.Patch, "/", a.Patch)

	for _, subAPI := range a.subAPIs {
		err := subAPI.Route(r)
		if err != nil {
			return fmt.Errorf("error creating routes for %q: %w", subAPI.Name(), err)
		}
	}

	a.doCustomRoutes(r, a.rootRoutes)
	a.doCustomRoutes(r, a.customRoutes)

	return nil
}

// Create a new router with API routes
func (a *API[T]) Router() (chi.Router, error) {
	r := chi.NewRouter()
	err := a.Route(r)
	return r, err
}

func (a *API[T]) doCustomRoutes(r chi.Router, routes []chi.Route) {
	for _, cr := range routes {
		for method, handler := range cr.Handlers {
			r.MethodFunc(method, cr.Pattern, handler.ServeHTTP)
		}
	}
}

func (a *API[T]) defaultGet() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())

		resource, httpErr := a.GetRequestedResource(r)
		if httpErr != nil {
			logger.Error("error getting requested resource", "error", httpErr.Error())
			return httpErr
		}

		render.Status(r, a.responseCodes[http.MethodGet])

		return a.responseWrapper(resource)
	})
}

func (a *API[T]) defaultGetAll() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())

		resources, err := a.Storage.GetAll(a.getAllFilter(r))
		if err != nil {
			logger.Error("error getting resources", "error", err)
			return InternalServerError(err)
		}
		logger.Debug("responding with resources", "count", len(resources))

		var resp render.Renderer
		if a.getAllResponseWrapper != nil {
			resp = a.getAllResponseWrapper(resources)
		} else {
			items := []render.Renderer{}
			for _, item := range resources {
				items = append(items, a.responseWrapper(item))
			}
			resp = &ResourceList[render.Renderer]{Items: items}
		}

		render.Status(r, a.responseCodes[http.MethodGet])

		return resp
	})
}

func (a *API[T]) defaultPost() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, resource T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		httpErr := a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing resource", "resource", resource)
		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		httpErr = a.afterCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		render.Status(r, a.responseCodes[http.MethodPost])

		return resource, nil
	})
}

func (a *API[T]) defaultPut() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, resource T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		if resource.GetID() != a.GetIDParam(r) {
			return *new(T), ErrInvalidRequest(fmt.Errorf("id must match URL path"))
		}

		httpErr := a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing resource", "resource", resource)
		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		httpErr = a.afterCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		render.Status(r, a.responseCodes[http.MethodPut])

		return resource, nil
	})
}

func (a *API[T]) defaultPatch() http.HandlerFunc {
	return a.ReadRequestBodyAndDo(func(r *http.Request, patchRequest T) (T, *ErrResponse) {
		logger := GetLoggerFromContext(r.Context())

		resource, httpErr := a.GetRequestedResource(r)
		if httpErr != nil {
			logger.Error("error getting requested resource", "error", httpErr.Error())
			return *new(T), httpErr
		}

		patcher, ok := any(resource).(Patcher[T])
		if !ok {
			return *new(T), ErrMethodNotAllowedResponse
		}

		httpErr = patcher.Patch(patchRequest)
		if httpErr != nil {
			logger.Error("error patching resource", "error", httpErr.Error())
			return *new(T), httpErr
		}

		httpErr = a.onCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		logger.Info("storing updated resource", "resource", resource)

		err := a.Storage.Set(resource)
		if err != nil {
			logger.Error("error storing updated resource", "error", err)
			return *new(T), InternalServerError(err)
		}

		httpErr = a.afterCreateOrUpdate(r, resource)
		if httpErr != nil {
			return *new(T), httpErr
		}

		render.Status(r, a.responseCodes[http.MethodPatch])

		return resource, nil
	})
}

func (a *API[T]) defaultDelete() http.HandlerFunc {
	return Handler(func(w http.ResponseWriter, r *http.Request) render.Renderer {
		logger := GetLoggerFromContext(r.Context())
		httpErr := a.beforeDelete(r)
		if httpErr != nil {
			logger.Error("error executing before func", "error", httpErr)
			return httpErr
		}

		id := a.GetIDParam(r)

		logger.Info("deleting resource", "id", id)

		err := a.Storage.Delete(id)
		if err != nil {
			logger.Error("error deleting resource", "error", err)

			if errors.Is(err, ErrNotFound) {
				return ErrNotFoundResponse
			}

			return InternalServerError(err)
		}

		httpErr = a.afterDelete(r)
		if httpErr != nil {
			logger.Error("error executing after func", "error", httpErr)
			return httpErr
		}

		w.WriteHeader(a.responseCodes[http.MethodDelete])
		return nil
	})
}

func routeIfNotNil(routeFunc func(string, http.HandlerFunc), pattern string, h http.HandlerFunc) {
	if h == nil {
		return
	}
	routeFunc(pattern, h)
}
