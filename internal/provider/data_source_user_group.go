package provider

import (
	"context"

	"github.com/rushiii/terraform-provider-zabbix/internal/zabbix"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = &userGroupDataSource{}
	_ datasource.DataSourceWithConfigure = &userGroupDataSource{}
)

type userGroupDataSource struct {
	client *zabbix.Client
}

type userGroupDataSourceModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

func NewUserGroupDataSource() datasource.DataSource {
	return &userGroupDataSource{}
}

func (d *userGroupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_group"
}

func (d *userGroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a Zabbix user group by name (e.g. built-in \"No access to the frontend\").",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "User group ID (usrgrpid).",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Exact name of the user group.",
			},
		},
	}
}

func (d *userGroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	providerData, ok := req.ProviderData.(*providerData)
	if !ok || providerData.Client == nil {
		resp.Diagnostics.AddError("Invalid provider", "Zabbix client unavailable.")
		return
	}
	d.client = providerData.Client
}

func (d *userGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config userGroupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := config.Name.ValueString()
	ids, err := d.client.UserGroupIDsByNames(ctx, []string{name})
	if err != nil {
		resp.Diagnostics.AddError("usergroup.get error", err.Error())
		return
	}
	if len(ids) == 0 {
		resp.Diagnostics.AddError("User group not found", "No user group with name: "+name)
		return
	}

	config.ID = types.StringValue(ids[0])
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
