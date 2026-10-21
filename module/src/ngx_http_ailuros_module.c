/*
 * Ailuros NGINX module.
 *
 * Asks Ailuros's HTTP forward-auth to allow or block each request on the
 * routes it is attached to.  The question is a subrequest to an internal
 * location that proxy_passes to Ailuros (headers only), sent from the
 * preaccess phase so the check is enforced next to auth_request.
 *
 * The handler is modelled on NGINX's ngx_http_auth_request_module; the
 * differences are explained where they occur.
 *
 * Licensed under the Apache License, Version 2.0.
 */

#include <ngx_config.h>
#include <ngx_core.h>
#include <ngx_http.h>


typedef struct {
    ngx_str_t                 uri;      /* empty: off; data NULL: unset */
    ngx_array_t              *vars;     /* of ngx_http_ailuros_variable_t */
    ngx_flag_t                open;     /* ailuros_request_unavailable */
} ngx_http_ailuros_loc_conf_t;


typedef struct {
    ngx_int_t                 status_index;  /* $ailuros_request_status */
} ngx_http_ailuros_main_conf_t;


typedef struct {
    ngx_uint_t                done;
    ngx_uint_t                status;
    ngx_http_request_t       *subrequest;
} ngx_http_ailuros_ctx_t;


typedef struct {
    ngx_int_t                 index;
    ngx_http_complex_value_t  value;
    ngx_http_set_variable_pt  set_handler;
} ngx_http_ailuros_variable_t;


static ngx_int_t ngx_http_ailuros_handler(ngx_http_request_t *r);
static ngx_int_t ngx_http_ailuros_done(ngx_http_request_t *r, void *data,
    ngx_int_t rc);
static ngx_uint_t ngx_http_ailuros_internal(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_http_ailuros_ctx_t *ctx);
static ngx_int_t ngx_http_ailuros_apply(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_uint_t status,
    ngx_uint_t internal);
static ngx_int_t ngx_http_ailuros_store_status(ngx_http_request_t *r,
    ngx_uint_t status);
static ngx_int_t ngx_http_ailuros_set_variables(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_http_ailuros_ctx_t *ctx);
static ngx_int_t ngx_http_ailuros_empty_variable(ngx_http_request_t *r,
    ngx_http_variable_value_t *v, uintptr_t data);
static ngx_int_t ngx_http_ailuros_add_variables(ngx_conf_t *cf);
static ngx_int_t ngx_http_ailuros_init(ngx_conf_t *cf);
static void *ngx_http_ailuros_create_main_conf(ngx_conf_t *cf);
static void *ngx_http_ailuros_create_loc_conf(ngx_conf_t *cf);
static char *ngx_http_ailuros_merge_loc_conf(ngx_conf_t *cf, void *parent,
    void *child);
static char *ngx_http_ailuros_request(ngx_conf_t *cf, ngx_command_t *cmd,
    void *conf);
static char *ngx_http_ailuros_request_set(ngx_conf_t *cf, ngx_command_t *cmd,
    void *conf);
static char *ngx_http_ailuros_request_unavailable(ngx_conf_t *cf,
    ngx_command_t *cmd, void *conf);


static ngx_command_t  ngx_http_ailuros_commands[] = {

    { ngx_string("ailuros_request"),
      NGX_HTTP_MAIN_CONF|NGX_HTTP_SRV_CONF|NGX_HTTP_LOC_CONF|NGX_CONF_TAKE1,
      ngx_http_ailuros_request,
      NGX_HTTP_LOC_CONF_OFFSET,
      0,
      NULL },

    { ngx_string("ailuros_request_set"),
      NGX_HTTP_MAIN_CONF|NGX_HTTP_SRV_CONF|NGX_HTTP_LOC_CONF|NGX_CONF_TAKE2,
      ngx_http_ailuros_request_set,
      NGX_HTTP_LOC_CONF_OFFSET,
      0,
      NULL },

    { ngx_string("ailuros_request_unavailable"),
      NGX_HTTP_MAIN_CONF|NGX_HTTP_SRV_CONF|NGX_HTTP_LOC_CONF|NGX_CONF_TAKE1,
      ngx_http_ailuros_request_unavailable,
      NGX_HTTP_LOC_CONF_OFFSET,
      0,
      NULL },

      ngx_null_command
};


static ngx_http_module_t  ngx_http_ailuros_module_ctx = {
    ngx_http_ailuros_add_variables,        /* preconfiguration */
    ngx_http_ailuros_init,                 /* postconfiguration */

    ngx_http_ailuros_create_main_conf,     /* create main configuration */
    NULL,                                  /* init main configuration */

    NULL,                                  /* create server configuration */
    NULL,                                  /* merge server configuration */

    ngx_http_ailuros_create_loc_conf,      /* create location configuration */
    ngx_http_ailuros_merge_loc_conf        /* merge location configuration */
};


ngx_module_t  ngx_http_ailuros_module = {
    NGX_MODULE_V1,
    &ngx_http_ailuros_module_ctx,          /* module context */
    ngx_http_ailuros_commands,             /* module directives */
    NGX_HTTP_MODULE,                       /* module type */
    NULL,                                  /* init master */
    NULL,                                  /* init module */
    NULL,                                  /* init process */
    NULL,                                  /* init thread */
    NULL,                                  /* exit thread */
    NULL,                                  /* exit process */
    NULL,                                  /* exit master */
    NGX_MODULE_V1_PADDING
};


static ngx_str_t  ngx_http_ailuros_status_name =
    ngx_string("ailuros_request_status");


/*
 * Preaccess phase handler, run by ngx_http_core_generic_phase:
 *
 *   NGX_DECLINED        next handler in the phase (the request continues)
 *   NGX_OK              skip to the next phase -- never returned here, since
 *                       it would also skip any preaccess handler that runs
 *                       after this one
 *   NGX_AGAIN           suspend; the phase engine calls this handler again
 *   NGX_HTTP_* / ERROR  finalize the request with that status
 *
 * Suspend and resume work as in auth_request: a waited subrequest is
 * created and NGX_AGAIN returned, so the generic checker leaves
 * r->phase_handler pointing at this handler and stops the phase loop.  When
 * the subrequest finishes, ngx_http_finalize_request() posts the parent,
 * whose write_event_handler is still ngx_http_core_run_phases, so this
 * handler is entered again and finds ctx->done set.
 *
 * Unlike the access phase checker, the generic checker does not skip
 * subrequests, so this handler checks r != r->main itself.
 */

static ngx_int_t
ngx_http_ailuros_handler(ngx_http_request_t *r)
{
    ngx_http_request_t           *sr;
    ngx_http_ailuros_ctx_t       *ctx;
    ngx_http_post_subrequest_t   *ps;
    ngx_http_ailuros_loc_conf_t  *alcf;

    if (r != r->main) {
        return NGX_DECLINED;
    }

    alcf = ngx_http_get_module_loc_conf(r, ngx_http_ailuros_module);

    if (alcf->uri.len == 0) {
        return NGX_DECLINED;
    }

    ngx_log_debug0(NGX_LOG_DEBUG_HTTP, r->connection->log, 0,
                   "ailuros request handler");

    ctx = ngx_http_get_module_ctx(r, ngx_http_ailuros_module);

    if (ctx != NULL) {
        if (!ctx->done) {
            return NGX_AGAIN;
        }

        /*
         * set variables explicitly, as auth_request does, so that they
         * are available after internal redirects
         */

        if (ngx_http_ailuros_store_status(r, ctx->status) != NGX_OK) {
            return NGX_ERROR;
        }

        if (ngx_http_ailuros_set_variables(r, alcf, ctx) != NGX_OK) {
            return NGX_ERROR;
        }

        return ngx_http_ailuros_apply(r, alcf, ctx->status,
                                      ngx_http_ailuros_internal(r, alcf, ctx));
    }

    ctx = ngx_pcalloc(r->pool, sizeof(ngx_http_ailuros_ctx_t));
    if (ctx == NULL) {
        return NGX_ERROR;
    }

    ps = ngx_palloc(r->pool, sizeof(ngx_http_post_subrequest_t));
    if (ps == NULL) {
        return NGX_ERROR;
    }

    ps->handler = ngx_http_ailuros_done;
    ps->data = ctx;

    if (ngx_http_subrequest(r, &alcf->uri, NULL, &sr, ps,
                            NGX_HTTP_SUBREQUEST_WAITED)
        != NGX_OK)
    {
        return NGX_ERROR;
    }

    /*
     * an empty request body: the client's body is never read for, or sent
     * to, Ailuros, and a body already read is left alone for the upstream
     */

    sr->request_body = ngx_pcalloc(r->pool, sizeof(ngx_http_request_body_t));
    if (sr->request_body == NULL) {
        return NGX_ERROR;
    }

    sr->header_only = 1;

    /*
     * no error_page redirection in the subrequest: an inherited
     * "error_page 502 = @fallback" or "error_page 502 =200 /page" would
     * otherwise turn the proxy's 502/504 (Ailuros down) into a 2xx;
     * intercepted upstream errors keep their own status
     */

    sr->error_page = 1;

    ctx->subrequest = sr;

    ngx_http_set_ctx(r, ctx, ngx_http_ailuros_module);

    return NGX_AGAIN;
}


/*
 * Called from ngx_http_finalize_request() of the subrequest, possibly more
 * than once (an error status is finalized first with the code in rc, then
 * again after the special response has set headers_out.status); the last
 * call wins.  If no status was set (an error before any response), the code
 * being finalized with is used when it is an HTTP status, and otherwise the
 * status stays 0, which ngx_http_ailuros_apply() treats as no decision.
 */

static ngx_int_t
ngx_http_ailuros_done(ngx_http_request_t *r, void *data, ngx_int_t rc)
{
    ngx_http_ailuros_ctx_t  *ctx = data;

    ngx_log_debug2(NGX_LOG_DEBUG_HTTP, r->connection->log, 0,
                   "ailuros request done s:%ui rc:%i",
                   r->headers_out.status, rc);

    ctx->done = 1;
    ctx->status = r->headers_out.status;

    if (ctx->status == 0
        && rc >= NGX_HTTP_SPECIAL_RESPONSE && rc < 600)
    {
        ctx->status = (ngx_uint_t) rc;
    }

    return rc;
}


/*
 * Whether the location the subrequest ended in (after any internal
 * redirect) is an "internal" location that is not named.  This does not
 * prove that the location proxies to Ailuros; it rejects the configurations
 * that would make a status come from somewhere else: a catch-all
 * "location /" answering a URI with no location of its own, a location that
 * clients can also request, and named locations (an auth subrequest has no
 * legitimate way to reach one once error_page is off, e.g. via try_files).
 * Anything rejected here is no decision.
 */

static ngx_uint_t
ngx_http_ailuros_internal(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_http_ailuros_ctx_t *ctx)
{
    const ngx_http_core_loc_conf_t  *clcf;

    clcf = ngx_http_get_module_loc_conf(ctx->subrequest, ngx_http_core_module);

    if (clcf->internal == 1 && !clcf->named) {
        return 1;
    }

    ngx_log_error(NGX_LOG_ERR, r->connection->log, 0,
                  "ailuros_request \"%V\" was answered (status %ui) by a "
                  "location that is not internal; the location must be "
                  "\"internal\", treating the answer as no decision",
                  &alcf->uri, ctx->status);

    return 0;
}


/*
 * Maps the subrequest's status to what the request gets:
 *
 *   2xx                continues
 *   400, 403, 413      that status
 *   503                503, whatever ailuros_request_unavailable says
 *   anything else      no decision: 503 when closed; when open, continues
 *                      with a warning
 *
 * "Anything else" includes 0 (no status), 1xx, 3xx, other 4xx (such as the
 * 404 of a missing internal location) and other 5xx (such as the proxy's
 * 502 and 504 when Ailuros is unreachable or times out).  Any status from
 * a location that is not internal is no decision too.
 */

static ngx_int_t
ngx_http_ailuros_apply(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_uint_t status, ngx_uint_t internal)
{
    if (!internal) {
        goto no_decision;
    }

    if (status >= NGX_HTTP_OK && status < NGX_HTTP_SPECIAL_RESPONSE) {
        return NGX_DECLINED;
    }

    switch (status) {

    case NGX_HTTP_BAD_REQUEST:
    case NGX_HTTP_FORBIDDEN:
    case NGX_HTTP_REQUEST_ENTITY_TOO_LARGE:
    case NGX_HTTP_SERVICE_UNAVAILABLE:
        return (ngx_int_t) status;

    default:
        break;
    }

no_decision:

    if (alcf->open) {
        ngx_log_error(NGX_LOG_WARN, r->connection->log, 0,
                      "ailuros_request \"%V\" gave no decision "
                      "(status %ui), letting the request through "
                      "(ailuros_request_unavailable open)",
                      &alcf->uri, status);
        return NGX_DECLINED;
    }

    ngx_log_error(NGX_LOG_ERR, r->connection->log, 0,
                  "ailuros_request \"%V\" gave no decision (status %ui), "
                  "rejecting the request with 503 "
                  "(ailuros_request_unavailable closed)",
                  &alcf->uri, status);

    return NGX_HTTP_SERVICE_UNAVAILABLE;
}


static ngx_int_t
ngx_http_ailuros_store_status(ngx_http_request_t *r, ngx_uint_t status)
{
    u_char                        *p;
    ngx_http_variable_value_t     *vv;
    ngx_http_ailuros_main_conf_t  *amcf;

    p = ngx_pnalloc(r->pool, NGX_INT_T_LEN);
    if (p == NULL) {
        return NGX_ERROR;
    }

    amcf = ngx_http_get_module_main_conf(r, ngx_http_ailuros_module);

    vv = &r->variables[amcf->status_index];

    vv->len = ngx_sprintf(p, "%ui", status) - p;
    vv->data = p;
    vv->valid = 1;
    vv->no_cacheable = 0;
    vv->not_found = 0;

    return NGX_OK;
}


static ngx_int_t
ngx_http_ailuros_set_variables(ngx_http_request_t *r,
    ngx_http_ailuros_loc_conf_t *alcf, ngx_http_ailuros_ctx_t *ctx)
{
    ngx_str_t                           val;
    ngx_http_variable_t                *v;
    ngx_http_ailuros_variable_t        *av;
    ngx_http_core_main_conf_t          *cmcf;
    const ngx_http_ailuros_variable_t  *last;

    if (alcf->vars == NULL) {
        return NGX_OK;
    }

    cmcf = ngx_http_get_module_main_conf(r, ngx_http_core_module);
    v = cmcf->variables.elts;

    av = alcf->vars->elts;
    last = av + alcf->vars->nelts;

    while (av < last) {
        ngx_http_variable_value_t  *vv = &r->variables[av->index];

        /* evaluated in the subrequest, for its $upstream_* variables */

        if (ngx_http_complex_value(ctx->subrequest, &av->value, &val)
            != NGX_OK)
        {
            return NGX_ERROR;
        }

        vv->valid = 1;
        vv->not_found = 0;
        vv->data = val.data;
        vv->len = val.len;

        if (av->set_handler) {
            /*
             * set_handler is only in cmcf->variables_keys, so it is
             * stored explicitly, as in auth_request_set
             */

            av->set_handler(r, vv, v[av->index].data);
        }

        av++;
    }

    return NGX_OK;
}


/* empty until set: $ailuros_request_status and ailuros_request_set targets */

static ngx_int_t
ngx_http_ailuros_empty_variable(ngx_http_request_t *r,
    ngx_http_variable_value_t *v, uintptr_t data)
{
    v->not_found = 1;

    return NGX_OK;
}


/*
 * Runs after the main conf is created (ngx_http_block creates all
 * configurations before calling preconfiguration).  The index is kept in
 * the main conf, not a static, so that each cycle's workers use the index
 * of the configuration they run, even after a reload that failed.
 */

static ngx_int_t
ngx_http_ailuros_add_variables(ngx_conf_t *cf)
{
    ngx_http_variable_t           *var;
    ngx_http_ailuros_main_conf_t  *amcf;

    amcf = ngx_http_conf_get_module_main_conf(cf, ngx_http_ailuros_module);

    var = ngx_http_add_variable(cf, &ngx_http_ailuros_status_name, 0);
    if (var == NULL) {
        return NGX_ERROR;
    }

    var->get_handler = ngx_http_ailuros_empty_variable;

    amcf->status_index = ngx_http_get_variable_index(cf,
                                               &ngx_http_ailuros_status_name);
    if (amcf->status_index == NGX_ERROR) {
        return NGX_ERROR;
    }

    return NGX_OK;
}


static ngx_int_t
ngx_http_ailuros_init(ngx_conf_t *cf)
{
    ngx_http_handler_pt        *h;
    ngx_http_core_main_conf_t  *cmcf;

    cmcf = ngx_http_conf_get_module_main_conf(cf, ngx_http_core_module);

    h = ngx_array_push(&cmcf->phases[NGX_HTTP_PREACCESS_PHASE].handlers);
    if (h == NULL) {
        return NGX_ERROR;
    }

    *h = ngx_http_ailuros_handler;

    return NGX_OK;
}


static void *
ngx_http_ailuros_create_main_conf(ngx_conf_t *cf)
{
    ngx_http_ailuros_main_conf_t  *amcf;

    amcf = ngx_pcalloc(cf->pool, sizeof(ngx_http_ailuros_main_conf_t));
    if (amcf == NULL) {
        return NULL;
    }

    amcf->status_index = NGX_ERROR;

    return amcf;
}


static void *
ngx_http_ailuros_create_loc_conf(ngx_conf_t *cf)
{
    ngx_http_ailuros_loc_conf_t  *conf;

    conf = ngx_pcalloc(cf->pool, sizeof(ngx_http_ailuros_loc_conf_t));
    if (conf == NULL) {
        return NULL;
    }

    /*
     * set by ngx_pcalloc():
     *
     *     conf->uri = { 0, NULL };
     */

    conf->vars = NGX_CONF_UNSET_PTR;
    conf->open = NGX_CONF_UNSET;

    return conf;
}


static char *
ngx_http_ailuros_merge_loc_conf(ngx_conf_t *cf, void *parent, void *child)
{
    ngx_http_ailuros_loc_conf_t  *prev = parent;
    ngx_http_ailuros_loc_conf_t  *conf = child;

    ngx_conf_merge_str_value(conf->uri, prev->uri, "");
    ngx_conf_merge_ptr_value(conf->vars, prev->vars, NULL);
    ngx_conf_merge_value(conf->open, prev->open, 0);

    return NGX_CONF_OK;
}


static char *
ngx_http_ailuros_request(ngx_conf_t *cf, ngx_command_t *cmd, void *conf)
{
    ngx_http_ailuros_loc_conf_t  *alcf = conf;

    ngx_str_t  *value;

    if (alcf->uri.data != NULL) {
        return "is duplicate";
    }

    value = cf->args->elts;

    if (ngx_strcmp(value[1].data, "off") == 0) {
        alcf->uri.len = 0;
        alcf->uri.data = (u_char *) "";

        return NGX_CONF_OK;
    }

    if (value[1].len == 0 || value[1].data[0] != '/') {
        ngx_conf_log_error(NGX_LOG_EMERG, cf, 0,
                           "invalid URI \"%V\" in \"%V\": it must be "
                           "\"off\" or start with \"/\"",
                           &value[1], &cmd->name);
        return NGX_CONF_ERROR;
    }

    alcf->uri = value[1];

    return NGX_CONF_OK;
}


static char *
ngx_http_ailuros_request_set(ngx_conf_t *cf, ngx_command_t *cmd, void *conf)
{
    ngx_http_ailuros_loc_conf_t  *alcf = conf;

    ngx_str_t                         *value;
    ngx_http_variable_t               *v;
    ngx_http_ailuros_variable_t       *av;
    ngx_http_compile_complex_value_t   ccv;

    value = cf->args->elts;

    if (value[1].len < 2 || value[1].data[0] != '$') {
        ngx_conf_log_error(NGX_LOG_EMERG, cf, 0,
                           "invalid variable name \"%V\" in \"%V\"",
                           &value[1], &cmd->name);
        return NGX_CONF_ERROR;
    }

    value[1].len--;
    value[1].data++;

    if (alcf->vars == NGX_CONF_UNSET_PTR) {
        alcf->vars = ngx_array_create(cf->pool, 1,
                                      sizeof(ngx_http_ailuros_variable_t));
        if (alcf->vars == NULL) {
            return NGX_CONF_ERROR;
        }
    }

    av = ngx_array_push(alcf->vars);
    if (av == NULL) {
        return NGX_CONF_ERROR;
    }

    v = ngx_http_add_variable(cf, &value[1], NGX_HTTP_VAR_CHANGEABLE);
    if (v == NULL) {
        return NGX_CONF_ERROR;
    }

    av->index = ngx_http_get_variable_index(cf, &value[1]);
    if (av->index == NGX_ERROR) {
        return NGX_CONF_ERROR;
    }

    if (v->get_handler == NULL) {
        v->get_handler = ngx_http_ailuros_empty_variable;
        v->data = (uintptr_t) av;
    }

    av->set_handler = v->set_handler;

    ngx_memzero(&ccv, sizeof(ngx_http_compile_complex_value_t));

    ccv.cf = cf;
    ccv.value = &value[2];
    ccv.complex_value = &av->value;

    if (ngx_http_compile_complex_value(&ccv) != NGX_OK) {
        return NGX_CONF_ERROR;
    }

    return NGX_CONF_OK;
}


static char *
ngx_http_ailuros_request_unavailable(ngx_conf_t *cf, ngx_command_t *cmd,
    void *conf)
{
    ngx_http_ailuros_loc_conf_t  *alcf = conf;

    ngx_str_t  *value;

    if (alcf->open != NGX_CONF_UNSET) {
        return "is duplicate";
    }

    value = cf->args->elts;

    if (ngx_strcmp(value[1].data, "closed") == 0) {
        alcf->open = 0;
        return NGX_CONF_OK;
    }

    if (ngx_strcmp(value[1].data, "open") == 0) {
        alcf->open = 1;
        return NGX_CONF_OK;
    }

    ngx_conf_log_error(NGX_LOG_EMERG, cf, 0,
                       "invalid value \"%V\" in \"%V\": it must be "
                       "\"closed\" or \"open\"",
                       &value[1], &cmd->name);

    return NGX_CONF_ERROR;
}
