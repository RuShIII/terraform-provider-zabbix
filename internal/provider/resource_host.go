package provider

import (
	"context"
	"strconv"

	"github.com/rushiii/terraform-provider-zabbix/internal/zabbix"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = &hostResource{}
	_ resource.ResourceWithConfigure   = &hostResource{}
	_ resource.ResourceWithImportState = &hostResource{}
	_ resource.ResourceWithModifyPlan  = &hostResource{}
)

type hostResource struct {
	client *zabbix.Client
}

type hostResourceModel struct {
	ID             types.String         `tfsdk:"id"`
	Name           types.String         `tfsdk:"name"`
	VisibleName    types.String         `tfsdk:"visible_name"`
	Enabled        types.Bool           `tfsdk:"enabled"`
	HostGroupIDs   types.Set            `tfsdk:"host_group_ids"`
	HostGroupNames types.Set            `tfsdk:"host_group_names"`
	TemplateIDs    types.Set            `tfsdk:"template_ids"`
	TemplateNames  types.Set            `tfsdk:"template_names"`
	Tags           types.Map            `tfsdk:"tags"`
	Interfaces     []hostInterfaceModel `tfsdk:"interfaces"`
}

type hostInterfaceModel struct {
	Type        types.Int64           `tfsdk:"type"` // 1=Agent,2=SNMP,3=IPMI,4=JMX
	Main        types.Bool            `tfsdk:"main"`
	UseIP       types.Bool            `tfsdk:"use_ip"`
	IP          types.String          `tfsdk:"ip"`
	DNS         types.String          `tfsdk:"dns"`
	Port        types.String          `tfsdk:"port"`
	SNMPDetails *hostSNMPDetailsModel `tfsdk:"snmp_details"`
}

type hostSNMPDetailsModel struct {
	Version   types.Int64  `tfsdk:"version"`
	Community types.String `tfsdk:"community"`
}

func NewHostResource() resource.Resource {
	return &hostResource{}
}

func (r *hostResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_host"
}

func (r *hostResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Zabbix host resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
				MarkdownDescription: "Internal Zabbix ID.",
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Host technical name (`host`).",
			},
			"visible_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Visible name (`name`) in Zabbix.",
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Whether the host is enabled.",
			},
			"host_group_ids": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Host group IDs.",
			},
			"host_group_names": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Host group names. Alternative to host_group_ids.",
			},
			"template_ids": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Linked template IDs.",
			},
			"template_names": schema.SetAttribute{
				Optional:            true,
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Template names. Alternative to template_ids.",
			},
			"tags": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Tags map (tag => value).",
			},
		},
		Blocks: map[string]schema.Block{
			"interfaces": schema.ListNestedBlock{
				MarkdownDescription: "Host interfaces.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"type": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "1=Agent, 2=SNMP, 3=IPMI, 4=JMX.",
						},
						"main": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(true),
						},
						"use_ip": schema.BoolAttribute{
							Optional: true,
							Computed: true,
							Default:  booldefault.StaticBool(true),
						},
						"ip": schema.StringAttribute{
							Optional: true,
						},
						"dns": schema.StringAttribute{
							Optional: true,
						},
						"port": schema.StringAttribute{
							Optional: true,
							Computed: true,
							Default:  stringdefault.StaticString("10050"),
						},
					},
					Blocks: map[string]schema.Block{
						"snmp_details": schema.SingleNestedBlock{
							MarkdownDescription: "SNMP details (v2 supported). Used mainly with type=2.",
							Attributes: map[string]schema.Attribute{
								"version": schema.Int64Attribute{
									Optional:            true,
									Computed:            true,
									Default:             int64default.StaticInt64(2),
									MarkdownDescription: "SNMP version (2 supported).",
								},
								"community": schema.StringAttribute{
									Optional:            true,
									Computed:            true,
									Default:             stringdefault.StaticString("{$SNMP_COMMUNITY}"),
									MarkdownDescription: "SNMP v1/v2c community.",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *hostResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *hostResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.client == nil || req.Plan.Raw.IsNull() {
		return
	}
	var plan hostResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	groupIDs, d := resolveHostGroupIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	templateIDs, d := resolveTemplateIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.HostGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)
	plan.TemplateIDs, _ = types.SetValueFrom(ctx, types.StringType, templateIDs)
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *hostResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hostResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	groupIDs, d := resolveHostGroupIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	templateIDs, d := resolveTemplateIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	tags, d := mapToTags(ctx, plan.Tags)
	resp.Diagnostics.Append(d...)
	interfaces, d := expandInterfaces(plan.Interfaces)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	hostID, err := r.client.HostCreate(ctx, zabbix.HostCreateRequest{
		Host:        plan.Name.ValueString(),
		Name:        nullableString(plan.VisibleName),
		Status:      boolToHostStatus(plan.Enabled),
		Interfaces:  interfaces,
		GroupIDs:    groupIDs,
		TemplateIDs: templateIDs,
		Tags:        tags,
	})
	if err != nil {
		resp.Diagnostics.AddError("host.create error", err.Error())
		return
	}

	plan.ID = types.StringValue(hostID)
	plan.HostGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)
	plan.TemplateIDs, _ = types.SetValueFrom(ctx, types.StringType, templateIDs)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hostResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	host, err := r.client.HostGetByID(ctx, state.ID.ValueString())
	if err != nil {
		if zabbix.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("host.get error", err.Error())
		return
	}

	state.Name = types.StringValue(host.Host)
	state.VisibleName = nullOrString(host.Name)
	state.Enabled = types.BoolValue(zabbix.StatusToEnabled(host.Status))
	state.Interfaces = flattenInterfaces(host.Interfaces)

	groupIDs := make([]string, 0, len(host.Groups))
	groupNames := make([]string, 0, len(host.Groups))
	for _, g := range host.Groups {
		groupIDs = append(groupIDs, g.GroupID)
		groupNames = append(groupNames, g.Name)
	}
	state.HostGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)
	state.HostGroupNames, _ = types.SetValueFrom(ctx, types.StringType, groupNames)

	templateIDs := make([]string, 0, len(host.ParentTemplates))
	templateNames := make([]string, 0, len(host.ParentTemplates))
	for _, t := range host.ParentTemplates {
		templateIDs = append(templateIDs, t.TemplateID)
		// Utiliser le nom technique (host) pour cohérence avec template_names en config
		// (ex. "Videoprojecteur" plutôt que "Vidéprojecteur").
		templateNames = append(templateNames, t.Host)
	}
	state.TemplateIDs, _ = types.SetValueFrom(ctx, types.StringType, templateIDs)
	state.TemplateNames, _ = types.SetValueFrom(ctx, types.StringType, templateNames)

	if len(host.Tags) == 0 {
		state.Tags = types.MapNull(types.StringType)
	} else {
		state.Tags, _ = tagsToMap(ctx, host.Tags)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hostResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hostResourceModel
	var state hostResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	groupIDs, d := resolveHostGroupIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	templateIDs, d := resolveTemplateIDs(ctx, r.client, plan)
	resp.Diagnostics.Append(d...)
	tags, d := mapToTags(ctx, plan.Tags)
	resp.Diagnostics.Append(d...)
	interfaces, d := expandInterfaces(plan.Interfaces)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	err := r.client.HostUpdate(ctx, state.ID.ValueString(), zabbix.HostUpdateRequest{
		Host:        plan.Name.ValueString(),
		Name:        nullableString(plan.VisibleName),
		Status:      boolToHostStatus(plan.Enabled),
		Interfaces:  interfaces,
		GroupIDs:    groupIDs,
		TemplateIDs: templateIDs,
		Tags:        tags,
	})
	if err != nil {
		resp.Diagnostics.AddError("host.update error", err.Error())
		return
	}

	plan.ID = state.ID
	plan.HostGroupIDs, _ = types.SetValueFrom(ctx, types.StringType, groupIDs)
	plan.TemplateIDs, _ = types.SetValueFrom(ctx, types.StringType, templateIDs)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hostResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hostResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.HostDelete(ctx, state.ID.ValueString())
	if err != nil && !zabbix.IsNotFound(err) {
		resp.Diagnostics.AddError("host.delete error", err.Error())
	}
}

func (r *hostResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func boolToHostStatus(value types.Bool) int {
	if !value.IsNull() && value.ValueBool() {
		return 0
	}
	return 1
}

func nullableString(value types.String) string {
	if value.IsNull() || value.IsUnknown() {
		return ""
	}
	return value.ValueString()
}

func nullOrString(value string) types.String {
	if value == "" {
		return types.StringNull()
	}
	return types.StringValue(value)
}

func setToStrings(ctx context.Context, value types.Set) ([]string, diag.Diagnostics) {
	var elems []types.String
	diags := value.ElementsAs(ctx, &elems, true)
	if diags.HasError() {
		return nil, diags
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		if e.IsNull() || e.IsUnknown() {
			continue
		}
		out = append(out, e.ValueString())
	}
	return out, diags
}

func setToStringsOptional(ctx context.Context, value types.Set) ([]string, diag.Diagnostics) {
	if value.IsNull() || value.IsUnknown() {
		return []string{}, nil
	}
	return setToStrings(ctx, value)
}

func resolveHostGroupIDs(ctx context.Context, client *zabbix.Client, plan hostResourceModel) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics

	ids, d := setToStringsOptional(ctx, plan.HostGroupIDs)
	diags.Append(d...)

	names, d := setToStringsOptional(ctx, plan.HostGroupNames)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}

	if len(ids) == 0 && len(names) == 0 {
		diags.AddAttributeError(
			path.Root("host_group_ids"),
			"Missing value",
			"Provide at least `host_group_ids` or `host_group_names`.",
		)
		return nil, diags
	}

	resolvedIDs := make([]string, 0, len(ids)+len(names))
	seen := map[string]struct{}{}

	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			resolvedIDs = append(resolvedIDs, id)
		}
	}

	if len(names) > 0 {
		byName, err := client.HostGroupIDsByNames(ctx, names)
		if err != nil {
			diags.AddError("Cannot resolve host groups", err.Error())
			return nil, diags
		}
		for _, id := range byName {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				resolvedIDs = append(resolvedIDs, id)
			}
		}
	}

	return resolvedIDs, diags
}

func resolveTemplateIDs(ctx context.Context, client *zabbix.Client, plan hostResourceModel) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics

	ids, d := setToStringsOptional(ctx, plan.TemplateIDs)
	diags.Append(d...)

	names, d := setToStringsOptional(ctx, plan.TemplateNames)
	diags.Append(d...)
	if diags.HasError() {
		return nil, diags
	}

	resolvedIDs := make([]string, 0, len(ids)+len(names))
	seen := map[string]struct{}{}

	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			resolvedIDs = append(resolvedIDs, id)
		}
	}

	if len(names) > 0 {
		byName, err := client.TemplateIDsByNames(ctx, names)
		if err != nil {
			diags.AddError("Cannot resolve templates", err.Error())
			return nil, diags
		}
		for _, id := range byName {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				resolvedIDs = append(resolvedIDs, id)
			}
		}
	}

	return resolvedIDs, diags
}

func mapToTags(ctx context.Context, value types.Map) ([]zabbix.Tag, diag.Diagnostics) {
	if value.IsNull() || value.IsUnknown() {
		return nil, nil
	}
	raw := map[string]string{}
	diags := value.ElementsAs(ctx, &raw, false)
	if diags.HasError() {
		return nil, diags
	}

	out := make([]zabbix.Tag, 0, len(raw))
	for tag, val := range raw {
		out = append(out, zabbix.Tag{Tag: tag, Value: val})
	}
	return out, diags
}

func tagsToMap(ctx context.Context, tags []zabbix.Tag) (types.Map, diag.Diagnostics) {
	raw := make(map[string]string, len(tags))
	for _, t := range tags {
		raw[t.Tag] = t.Value
	}
	return types.MapValueFrom(ctx, types.StringType, raw)
}

func expandInterfaces(interfaces []hostInterfaceModel) ([]zabbix.HostInterface, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := make([]zabbix.HostInterface, 0, len(interfaces))

	for index, it := range interfaces {
		useIP := !it.UseIP.IsNull() && it.UseIP.ValueBool()

		if useIP && (it.IP.IsNull() || it.IP.ValueString() == "") {
			diags.AddAttributeError(
				path.Root("interfaces").AtListIndex(index).AtName("ip"),
				"Missing value",
				"`ip` is required when `use_ip=true`.",
			)
			continue
		}
		if !useIP && (it.DNS.IsNull() || it.DNS.ValueString() == "") {
			diags.AddAttributeError(
				path.Root("interfaces").AtListIndex(index).AtName("dns"),
				"Missing value",
				"`dns` is required when `use_ip=false`.",
			)
			continue
		}

		port := "10050"
		if !it.Port.IsNull() && it.Port.ValueString() != "" {
			if _, err := strconv.Atoi(it.Port.ValueString()); err == nil {
				port = it.Port.ValueString()
			}
		}

		iface := zabbix.HostInterface{
			Type:  int(it.Type.ValueInt64()),
			Main:  boolToInt(!it.Main.IsNull() && it.Main.ValueBool()),
			UseIP: boolToInt(useIP),
			IP:    nullableString(it.IP),
			DNS:   nullableString(it.DNS),
			Port:  port,
		}

		if iface.Type == 2 {
			details := expandSNMPDetails(it.SNMPDetails)
			if details.Version != 2 {
				diags.AddAttributeError(
					path.Root("interfaces").AtListIndex(index).AtName("snmp_details").AtName("version"),
"Unsupported SNMP version",
				"This provider currently only supports SNMP v2 for hosts.",
				)
				continue
			}
			iface.Details = details
		}

		out = append(out, iface)
	}

	return out, diags
}

func flattenInterfaces(interfaces []zabbix.HostInterface) []hostInterfaceModel {
	out := make([]hostInterfaceModel, 0, len(interfaces))
	for _, it := range interfaces {
		out = append(out, hostInterfaceModel{
			Type:  types.Int64Value(int64(it.Type)),
			Main:  types.BoolValue(it.Main == 1),
			UseIP: types.BoolValue(it.UseIP == 1),
			IP:    nullOrString(it.IP),
			DNS:   nullOrString(it.DNS),
			Port:  types.StringValue(it.Port),
		})
		if it.Type == 2 && it.Details != nil && int(it.Details.Version) != 0 {
			out[len(out)-1].SNMPDetails = &hostSNMPDetailsModel{
				Version:   types.Int64Value(int64(it.Details.Version)),
				Community: nullOrString(it.Details.Community),
			}
		}
	}
	return out
}

func expandSNMPDetails(in *hostSNMPDetailsModel) *zabbix.SNMPDetails {
	if in == nil {
		return &zabbix.SNMPDetails{
			Version:   2,
			Community: "{$SNMP_COMMUNITY}",
		}
	}

	version := int(in.Version.ValueInt64())
	if in.Version.IsNull() || in.Version.IsUnknown() || version == 0 {
		version = 2
	}

	community := nullableString(in.Community)
	if community == "" {
		community = "{$SNMP_COMMUNITY}"
	}

	return &zabbix.SNMPDetails{
		Version:   zabbix.FlexIntFrom(version),
		Community: community,
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
