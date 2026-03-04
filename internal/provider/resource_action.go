package provider

import (
	"context"

	"github.com/rushiii/terraform-provider-zabbix/internal/zabbix"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &actionResource{}
	_ resource.ResourceWithConfigure   = &actionResource{}
	_ resource.ResourceWithImportState = &actionResource{}
)

type actionResource struct {
	client *zabbix.Client
}

type actionResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	UserGroupIDs    types.Set    `tfsdk:"user_group_ids"`
	TriggerNameLike types.Set    `tfsdk:"trigger_name_like"`
	Subject         types.String `tfsdk:"subject"`
	Message         types.String `tfsdk:"message"`
	Enabled         types.Bool   `tfsdk:"enabled"`
	EscPeriod       types.String `tfsdk:"esc_period"`
}

func NewActionResource() resource.Resource {
	return &actionResource{}
}

func (r *actionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_action"
}

func (r *actionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Zabbix trigger action: send notifications (e.g. email) when a trigger fires (problem).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Action name (e.g. \"Envoyer mail en cas de problème\").",
			},
			"user_group_ids": schema.SetAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "IDs of user groups to notify (e.g. Zabbix administrators). Use data source or variable.",
			},
			"trigger_name_like": schema.SetAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "If set, action runs only when trigger name (description) contains any of these strings (e.g. [\"Lampe\", \"Laser\"] for Videoprojecteur Lampe/Laser).",
			},
			"subject": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Email subject. Supports macros: {TRIGGER.NAME}, {HOST.NAME}, {EVENT.STATUS}, etc.",
			},
			"message": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Email body. Supports Zabbix macros.",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the action is enabled.",
			},
			"esc_period": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("1h"),
				MarkdownDescription: "Minimum interval between notifications (e.g. \"1h\", \"60s\").",
			},
		},
	}
}

func (r *actionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *actionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan actionResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	groupIDs, d := setToStrings(ctx, plan.UserGroupIDs)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	triggerNameLike, _ := setToStringsOptional(ctx, plan.TriggerNameLike)

	id, err := r.client.ActionCreate(ctx, zabbix.ActionCreateRequest{
		Name:            plan.Name.ValueString(),
		UserGroupIDs:    groupIDs,
		TriggerNameLike: triggerNameLike,
		Subject:         plan.Subject.ValueString(),
		Message:         plan.Message.ValueString(),
		Enabled:         plan.Enabled.ValueBool(),
		EscPeriod:       plan.EscPeriod.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("action.create error", err.Error())
		return
	}

	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *actionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state actionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	action, err := r.client.ActionGetByID(ctx, state.ID.ValueString())
	if err != nil {
		if zabbix.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("action.get error", err.Error())
		return
	}

	state.Name = types.StringValue(action.Name)
	state.Subject = types.StringValue(action.DefShortData)
	state.Message = types.StringValue(action.DefLongData)
	state.Enabled = types.BoolValue(action.Status == "0")
	state.EscPeriod = types.StringValue(action.EscPeriod)

	groupIDs := make([]string, 0)
	for _, op := range action.Operations {
		for _, g := range op.OpmessageGrp {
			if g.UsrgrpID != "" {
				groupIDs = append(groupIDs, g.UsrgrpID)
			}
		}
	}
	state.UserGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)

	// Rebuild trigger_name_like from conditions (conditiontype 2 = trigger name)
	triggerNameLike := make([]string, 0)
	for _, c := range action.Conditions {
		if c.ConditionType == "2" && c.Value != "" {
			triggerNameLike = append(triggerNameLike, c.Value)
		}
	}
	if len(triggerNameLike) > 0 {
		state.TriggerNameLike, _ = types.SetValueFrom(ctx, types.StringType, triggerNameLike)
	} else {
		state.TriggerNameLike = types.SetNull(types.StringType)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *actionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan actionResourceModel
	var state actionResourceModel
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
	triggerNameLike, _ := setToStringsOptional(ctx, plan.TriggerNameLike)

	err := r.client.ActionUpdate(ctx, state.ID.ValueString(), zabbix.ActionCreateRequest{
		Name:            plan.Name.ValueString(),
		UserGroupIDs:    groupIDs,
		TriggerNameLike: triggerNameLike,
		Subject:         plan.Subject.ValueString(),
		Message:         plan.Message.ValueString(),
		Enabled:         plan.Enabled.ValueBool(),
		EscPeriod:       plan.EscPeriod.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("action.update error", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *actionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state actionResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.ActionDelete(ctx, state.ID.ValueString())
	if err != nil && !zabbix.IsNotFound(err) {
		resp.Diagnostics.AddError("action.delete error", err.Error())
	}
}

func (r *actionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
