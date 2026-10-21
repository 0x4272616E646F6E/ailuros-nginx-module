#include <ngx_config.h>
#include <ngx_core.h>
#include <ngx_http.h>

typedef struct {
    ngx_flag_t enabled;
} ngx_http_ailuros_loc_conf_t;

static ngx_int_t ngx_http_ailuros_init(ngx_conf_t *cf);
static void *ngx_http_ailuros_create_loc_conf(ngx_conf_t *cf);
static char *ngx_http_ailuros_merge_loc_conf(ngx_conf_t *cf, void *parent,
    void *child);
static ngx_int_t ngx_http_ailuros_header_filter(ngx_http_request_t *r);

static ngx_command_t ngx_http_ailuros_commands[] = {
    {
        ngx_string("ailuros"),
        NGX_HTTP_MAIN_CONF | NGX_HTTP_SRV_CONF | NGX_HTTP_LOC_CONF
            | NGX_CONF_FLAG,
        ngx_conf_set_flag_slot,
        NGX_HTTP_LOC_CONF_OFFSET,
        offsetof(ngx_http_ailuros_loc_conf_t, enabled),
        NULL
    },
    ngx_null_command
};

static ngx_http_module_t ngx_http_ailuros_module_ctx = {
    NULL,
    ngx_http_ailuros_init,
    NULL,
    NULL,
    NULL,
    NULL,
    ngx_http_ailuros_create_loc_conf,
    ngx_http_ailuros_merge_loc_conf
};

ngx_module_t ngx_http_ailuros_module = {
    NGX_MODULE_V1,
    &ngx_http_ailuros_module_ctx,
    ngx_http_ailuros_commands,
    NGX_HTTP_MODULE,
    NULL,
    NULL,
    NULL,
    NULL,
    NULL,
    NULL,
    NULL,
    NGX_MODULE_V1_PADDING
};

static ngx_http_output_header_filter_pt ngx_http_next_header_filter;

static ngx_int_t
ngx_http_ailuros_init(ngx_conf_t *cf)
{
    ngx_http_next_header_filter = ngx_http_top_header_filter;
    ngx_http_top_header_filter = ngx_http_ailuros_header_filter;
    return NGX_OK;
}

static ngx_int_t
ngx_http_ailuros_header_filter(ngx_http_request_t *r)
{
    ngx_http_ailuros_loc_conf_t *conf;
    ngx_table_elt_t *header;

    conf = ngx_http_get_module_loc_conf(r, ngx_http_ailuros_module);
    if (conf == NULL || !conf->enabled || r->headers_out.status == NGX_HTTP_NO_CONTENT) {
        return ngx_http_next_header_filter(r);
    }

    header = ngx_list_push(&r->headers_out.headers);
    if (header == NULL) {
        return NGX_ERROR;
    }

    header->hash = 1;
    ngx_str_set(&header->key, "X-Ailuros-Module");
    ngx_str_set(&header->value, "enabled");

    return ngx_http_next_header_filter(r);
}

static void *
ngx_http_ailuros_create_loc_conf(ngx_conf_t *cf)
{
    ngx_http_ailuros_loc_conf_t *conf;

    conf = ngx_pcalloc(cf->pool, sizeof(ngx_http_ailuros_loc_conf_t));
    if (conf == NULL) {
        return NULL;
    }

    conf->enabled = NGX_CONF_UNSET;
    return conf;
}

static char *
ngx_http_ailuros_merge_loc_conf(ngx_conf_t *cf, void *parent, void *child)
{
    ngx_http_ailuros_loc_conf_t *prev = parent;
    ngx_http_ailuros_loc_conf_t *conf = child;

    ngx_conf_merge_value(conf->enabled, prev->enabled, 0);
    return NGX_CONF_OK;
}
