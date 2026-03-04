package provider

import (
	"context"

	"github.com/rushiii/terraform-provider-zabbix/internal/zabbix"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &userResource{}
	_ resource.ResourceWithConfigure   = &userResource{}
	_ resource.ResourceWithImportState = &userResource{}
)

type userResource struct {
	client *zabbix.Client
}

type userResourceModel struct {
	ID            types.String `tfsdk:"id"`
	Username      types.String `tfsdk:"username"`
	Name          types.String `tfsdk:"name"`
	Password      types.String `tfsdk:"password"`
	UserGroupIDs  types.Set    `tfsdk:"user_group_ids"`
	RoleID        types.String `tfsdk:"role_id"`
	Email         types.String `tfsdk:"email"`
}

func NewUserResource() resource.Resource {
	return &userResource{}
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Zabbix user (e.g. for receiving notifications by email).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"username": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Login name.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Display name.",
			},
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "User password. Omit or leave empty to create a user without password (e.g. notification-only); on update, leave unchanged by not setting this attribute.",
			},
			"user_group_ids": schema.SetAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "IDs of user groups this user belongs to.",
			},
			"role_id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("1"),
				MarkdownDescription: "Role: 1=User, 2=Admin, 3=Super admin.",
			},
			"email": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Email address for notifications (adds Email media to the user).",
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	providerData, ok := req.ProviderData.(*providerData)
	if !ok || providerData.Client == nil {
		resp.Diagnostics.AddError("Invalid provider", "Zabbix client unavailable.")
		return
	}
	r.client = providerData.Client
}

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	groupIDs, d := setToStrings(ctx, plan.UserGroupIDs)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.client.UserCreate(ctx, zabbix.UserCreateRequest{
		Username:   plan.Username.ValueString(),
		Name:       plan.Name.ValueString(),
		Password:   plan.Password.ValueString(),
		UserGrpIDs: groupIDs,
		RoleID:     plan.RoleID.ValueString(),
		Email:      plan.Email.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("user.create error", err.Error())
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	u, err := r.client.UserGetByID(ctx, state.ID.ValueString())
	if err != nil {
		if zabbix.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("user.get error", err.Error())
		return
	}
	state.Username = types.StringValue(u.Username)
	state.Name = types.StringValue(u.Name)
	if u.RoleID != "" {
		state.RoleID = types.StringValue(u.RoleID)
	}
	state.Email = types.StringNull()
	for _, m := range u.Medias {
		if m.MediaTypeID == "1" {
			state.Email = types.StringValue(string(m.SendTo))
			break
		}
	}
	groupIDs := make([]string, 0, len(u.Usrgrps))
	for _, g := range u.Usrgrps {
		groupIDs = append(groupIDs, g.UsrgrpID)
	}
	state.UserGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)
	// password is not returned by API; keep from state
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userResourceModel
	var state userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	groupIDs, d := setToStrings(ctx, plan.UserGroupIDs)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	reqUpdate := zabbix.UserCreateRequest{
		Username:   plan.Username.ValueString(),
		Name:       plan.Name.ValueString(),
		UserGrpIDs: groupIDs,
		RoleID:     plan.RoleID.ValueString(),
		Email:      plan.Email.ValueString(),
	}
	if !plan.Password.IsNull() && plan.Password.ValueString() != "" {
		reqUpdate.Password = plan.Password.ValueString()
	}
	if err := r.client.UserUpdate(ctx, state.ID.ValueString(), reqUpdate); err != nil {
		resp.Diagnostics.AddError("user.update error", err.Error())
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.UserDelete(ctx, state.ID.ValueString())
	if err != nil && !zabbix.IsNotFound(err) {
		resp.Diagnostics.AddError("user.delete error", err.Error())
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
