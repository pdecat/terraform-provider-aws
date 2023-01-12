package emr

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/emr"
	"github.com/hashicorp/aws-sdk-go-base/v2/awsv1shim/v2/tfawserr"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/structure"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-aws/internal/conns"
	"github.com/hashicorp/terraform-provider-aws/internal/tfresource"
	"github.com/hashicorp/terraform-provider-aws/internal/verify"
)

const (
	instanceGroupCreateTimeout = 30 * time.Minute
	instanceGroupUpdateTimeout = 30 * time.Minute
)

func ResourceInstanceGroup() *schema.Resource {
	return &schema.Resource{
		Create: resourceInstanceGroupCreate,
		Read:   resourceInstanceGroupRead,
		Update: resourceInstanceGroupUpdate,
		Delete: resourceInstanceGroupDelete,
		Importer: &schema.ResourceImporter{
			State: func(d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
				idParts := strings.Split(d.Id(), "/")
				if len(idParts) != 2 || idParts[0] == "" || idParts[1] == "" {
					return nil, fmt.Errorf("Unexpected format of ID (%q), expected cluster-id/ig-id", d.Id())
				}
				clusterID := idParts[0]
				resourceID := idParts[1]
				d.Set("cluster_id", clusterID)
				d.SetId(resourceID)
				return []*schema.ResourceData{d}, nil
			},
		},
		Schema: map[string]*schema.Schema{
			"autoscaling_policy": {
				Type:             schema.TypeString,
				Optional:         true,
				DiffSuppressFunc: verify.SuppressEquivalentJSONDiffs,
				ValidateFunc:     validation.StringIsJSON,
			},
			"bid_price": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},
			"cluster_id": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"configurations_json": {
				Type:             schema.TypeString,
				Optional:         true,
				ValidateFunc:     validation.StringIsJSON,
				DiffSuppressFunc: verify.SuppressEquivalentJSONDiffs,
				StateFunc: func(v interface{}) string {
					json, _ := structure.NormalizeJsonString(v)
					return json
				},
			},
			"ebs_config": {
				Type:     schema.TypeSet,
				Optional: true,
				Computed: true,
				ForceNew: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"iops": {
							Type:     schema.TypeInt,
							Optional: true,
							ForceNew: true,
						},
						"size": {
							Type:     schema.TypeInt,
							Required: true,
							ForceNew: true,
						},
						"type": {
							Type:         schema.TypeString,
							Required:     true,
							ForceNew:     true,
							ValidateFunc: validEBSVolumeType(),
						},
						"volumes_per_instance": {
							Type:     schema.TypeInt,
							Optional: true,
							ForceNew: true,
							Default:  1,
						},
					},
				},
				Set: resourceClusterEBSHashConfig,
			},
			"ebs_optimized": {
				Type:     schema.TypeBool,
				Optional: true,
				ForceNew: true,
			},
			"instance_count": {
				Type:     schema.TypeInt,
				Optional: true,
				Computed: true,
			},
			"instance_type": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"name": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
			},
			"running_instance_count": {
				Type:     schema.TypeInt,
				Computed: true,
			},
			"status": {
				Type:     schema.TypeString,
				Computed: true,
			},
		},
	}
}

func resourceInstanceGroupCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).EMRConn()

	instanceRole := emr.InstanceGroupTypeTask
	groupConfig := &emr.InstanceGroupConfig{
		EbsConfiguration: readEBSConfig(d),
		InstanceRole:     aws.String(instanceRole),
		InstanceType:     aws.String(d.Get("instance_type").(string)),
		Name:             aws.String(d.Get("name").(string)),
	}

	if v, ok := d.GetOk("autoscaling_policy"); ok {
		var autoScalingPolicy *emr.AutoScalingPolicy

		if err := json.Unmarshal([]byte(v.(string)), &autoScalingPolicy); err != nil {
			return fmt.Errorf("[DEBUG] error parsing Auto Scaling Policy %s", err)
		}
		groupConfig.AutoScalingPolicy = autoScalingPolicy
	}

	if v, ok := d.GetOk("configurations_json"); ok {
		info, err := structure.NormalizeJsonString(v)
		if err != nil {
			return fmt.Errorf("configurations_json contains an invalid JSON: %s", err)
		}
		groupConfig.Configurations, err = expandConfigurationJSON(info)
		if err != nil {
			return fmt.Errorf("Error reading EMR configurations_json: %s", err)
		}
	}

	if v, ok := d.GetOk("instance_count"); ok {
		groupConfig.InstanceCount = aws.Int64(int64(v.(int)))
	} else {
		groupConfig.InstanceCount = aws.Int64(1)
	}

	groupConfig.Market = aws.String(emr.MarketTypeOnDemand)
	if v, ok := d.GetOk("bid_price"); ok {
		groupConfig.BidPrice = aws.String(v.(string))
		groupConfig.Market = aws.String(emr.MarketTypeSpot)
	}

	params := &emr.AddInstanceGroupsInput{
		InstanceGroups: []*emr.InstanceGroupConfig{groupConfig},
		JobFlowId:      aws.String(d.Get("cluster_id").(string)),
	}

	resp, err := conn.AddInstanceGroups(params)
	if err != nil {
		return fmt.Errorf("creating EMR Instance Group: %w", err)
	}

	if resp == nil || len(resp.InstanceGroupIds) == 0 {
		return errors.New("creating EMR Instance Group: empty response")
	}
	d.SetId(aws.StringValue(resp.InstanceGroupIds[0]))

	if err := waitForInstanceGroupStateRunning(conn, d.Get("cluster_id").(string), d.Id(), instanceGroupCreateTimeout); err != nil {
		return fmt.Errorf("creating EMR Instance Group (%s): waiting for completion: %s", d.Id(), err)
	}

	return resourceInstanceGroupRead(d, meta)
}

func resourceInstanceGroupRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).EMRConn()

	ig, err := FetchInstanceGroup(conn, d.Get("cluster_id").(string), d.Id())

	if tfresource.NotFound(err) {
		log.Printf("[DEBUG] EMR Instance Group (%s) not found, removing", d.Id())
		d.SetId("")
		return nil
	}

	if err != nil {
		if tfawserr.ErrMessageContains(err, emr.ErrCodeInvalidRequestException, "is not valid") {
			log.Printf("[DEBUG] EMR Cluster corresponding to Instance Group (%s) not found, removing", d.Id())
			d.SetId("")
			return nil
		}
		return fmt.Errorf("reading EMR Instance Group (%s): %s", d.Id(), err)
	}

	if ig.Status != nil {
		switch aws.StringValue(ig.Status.State) {
		case emr.InstanceGroupStateTerminating:
			fallthrough
		case emr.InstanceGroupStateTerminated:
			log.Printf("[DEBUG] EMR Instance Group (%s) terminated, removing", d.Id())
			d.SetId("")
			return nil
		}
	}

	switch {
	case len(ig.Configurations) > 0:
		configOut, err := flattenConfigurationJSON(ig.Configurations)
		if err != nil {
			return fmt.Errorf("Error reading EMR instance group configurations: %s", err)
		}
		if err := d.Set("configurations_json", configOut); err != nil {
			return fmt.Errorf("Error setting EMR configurations_json for instance group (%s): %s", d.Id(), err)
		}
	default:
		d.Set("configurations_json", "")
	}

	autoscalingPolicyString, err := flattenAutoScalingPolicyDescription(ig.AutoScalingPolicy)

	if err != nil {
		return fmt.Errorf("reading EMR Instance Group (%s): %s", d.Id(), err)
	}

	d.Set("autoscaling_policy", autoscalingPolicyString)

	d.Set("bid_price", ig.BidPrice)
	if err := d.Set("ebs_config", flattenEBSConfig(ig.EbsBlockDevices)); err != nil {
		return fmt.Errorf("error setting ebs_config: %s", err)
	}
	d.Set("ebs_optimized", ig.EbsOptimized)
	d.Set("instance_count", ig.RequestedInstanceCount)
	d.Set("instance_type", ig.InstanceType)
	d.Set("name", ig.Name)
	d.Set("running_instance_count", ig.RunningInstanceCount)

	if ig.Status != nil {
		d.Set("status", ig.Status.State)
	}

	return nil
}

func resourceInstanceGroupUpdate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).EMRConn()

	log.Printf("[DEBUG] Modify EMR task group")
	if d.HasChanges("instance_count", "configurations_json") {
		instanceGroupModifyConfig := emr.InstanceGroupModifyConfig{
			InstanceGroupId: aws.String(d.Id()),
		}

		if d.HasChange("instance_count") {
			instanceCount := d.Get("instance_count").(int)
			instanceGroupModifyConfig.InstanceCount = aws.Int64(int64(instanceCount))
		}
		if d.HasChange("configurations_json") {
			if v, ok := d.GetOk("configurations_json"); ok {
				info, err := structure.NormalizeJsonString(v)
				if err != nil {
					return fmt.Errorf("configurations_json contains an invalid JSON: %s", err)
				}
				instanceGroupModifyConfig.Configurations, err = expandConfigurationJSON(info)
				if err != nil {
					return fmt.Errorf("Error reading EMR configurations_json: %s", err)
				}
			}
		}
		params := &emr.ModifyInstanceGroupsInput{
			InstanceGroups: []*emr.InstanceGroupModifyConfig{
				&instanceGroupModifyConfig,
			},
		}

		_, err := conn.ModifyInstanceGroups(params)
		if err != nil {
			return fmt.Errorf("error modifying EMR Instance Group (%s): %s", d.Id(), err)
		}

		if err := waitForInstanceGroupStateRunning(conn, d.Get("cluster_id").(string), d.Id(), instanceGroupUpdateTimeout); err != nil {
			return fmt.Errorf("error waiting for EMR Instance Group (%s) modification: %s", d.Id(), err)
		}
	}

	if d.HasChange("autoscaling_policy") {
		var autoScalingPolicy *emr.AutoScalingPolicy

		if err := json.Unmarshal([]byte(d.Get("autoscaling_policy").(string)), &autoScalingPolicy); err != nil {
			return fmt.Errorf("error parsing EMR Auto Scaling Policy JSON for update: %s", err)
		}

		putAutoScalingPolicy := &emr.PutAutoScalingPolicyInput{
			ClusterId:         aws.String(d.Get("cluster_id").(string)),
			AutoScalingPolicy: autoScalingPolicy,
			InstanceGroupId:   aws.String(d.Id()),
		}

		if _, err := conn.PutAutoScalingPolicy(putAutoScalingPolicy); err != nil {
			return fmt.Errorf("error updating autoscaling policy for instance group %q: %s", d.Id(), err)
		}
	}

	return resourceInstanceGroupRead(d, meta)
}

func resourceInstanceGroupDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*conns.AWSClient).EMRConn()

	log.Printf("[WARN] AWS EMR Instance Group does not support DELETE; resizing cluster to zero before removing from state")
	params := &emr.ModifyInstanceGroupsInput{
		InstanceGroups: []*emr.InstanceGroupModifyConfig{
			{
				InstanceGroupId: aws.String(d.Id()),
				InstanceCount:   aws.Int64(0),
			},
		},
	}

	if _, err := conn.ModifyInstanceGroups(params); err != nil {
		return fmt.Errorf("error draining EMR Instance Group (%s): %s", d.Id(), err)
	}
	return nil
}

func instanceGroupStateRefresh(conn *emr.EMR, clusterID, groupID string) resource.StateRefreshFunc {
	return func() (interface{}, string, error) {
		ig, err := FetchInstanceGroup(conn, clusterID, groupID)
		if err != nil {
			return nil, "Not Found", err
		}

		if ig.Status == nil || ig.Status.State == nil {
			log.Printf("[WARN] ERM Instance Group found, but without state")
			return nil, "Undefined", fmt.Errorf("Undefined EMR Cluster Instance Group state")
		}

		return ig, *ig.Status.State, nil
	}
}

func FetchInstanceGroup(conn *emr.EMR, clusterID, groupID string) (*emr.InstanceGroup, error) {
	input := &emr.ListInstanceGroupsInput{ClusterId: aws.String(clusterID)}

	var groups []*emr.InstanceGroup
	err := conn.ListInstanceGroupsPages(input, func(page *emr.ListInstanceGroupsOutput, lastPage bool) bool {
		groups = append(groups, page.InstanceGroups...)

		return !lastPage
	})

	if err != nil {
		return nil, fmt.Errorf("unable to retrieve EMR Cluster (%q): %s", clusterID, err)
	}

	if len(groups) == 0 {
		return nil, fmt.Errorf("no instance groups found for EMR Cluster (%s)", clusterID)
	}

	var ig *emr.InstanceGroup
	for _, group := range groups {
		if groupID == aws.StringValue(group.Id) {
			ig = group
			break
		}
	}

	if ig == nil {
		return nil, &resource.NotFoundError{}
	}

	return ig, nil
}

// readEBSConfig populates an emr.EbsConfiguration struct
func readEBSConfig(d *schema.ResourceData) *emr.EbsConfiguration {
	result := &emr.EbsConfiguration{}
	if v, ok := d.GetOk("ebs_optimized"); ok {
		result.EbsOptimized = aws.Bool(v.(bool))
	}

	ebsConfigs := make([]*emr.EbsBlockDeviceConfig, 0)
	if rawConfig, ok := d.GetOk("ebs_config"); ok {
		configList := rawConfig.(*schema.Set).List()
		for _, config := range configList {
			conf := config.(map[string]interface{})
			ebs := &emr.EbsBlockDeviceConfig{}
			volumeSpec := &emr.VolumeSpecification{
				SizeInGB:   aws.Int64(int64(conf["size"].(int))),
				VolumeType: aws.String(conf["type"].(string)),
			}
			if v, ok := conf["iops"].(int); ok && v != 0 {
				volumeSpec.Iops = aws.Int64(int64(v))
			}
			if v, ok := conf["volumes_per_instance"].(int); ok && v != 0 {
				ebs.VolumesPerInstance = aws.Int64(int64(v))
			}
			ebs.VolumeSpecification = volumeSpec
			ebsConfigs = append(ebsConfigs, ebs)
		}
	}
	result.EbsBlockDeviceConfigs = ebsConfigs
	return result
}

func waitForInstanceGroupStateRunning(conn *emr.EMR, clusterID string, instanceGroupID string, timeout time.Duration) error {
	stateConf := &resource.StateChangeConf{
		Pending: []string{
			emr.InstanceGroupStateBootstrapping,
			emr.InstanceGroupStateProvisioning,
			emr.InstanceGroupStateReconfiguring,
			emr.InstanceGroupStateResizing,
		},
		Target:     []string{emr.InstanceGroupStateRunning},
		Refresh:    instanceGroupStateRefresh(conn, clusterID, instanceGroupID),
		Timeout:    timeout,
		Delay:      10 * time.Second,
		MinTimeout: 3 * time.Second,
	}

	_, err := stateConf.WaitForState()

	return err
}
