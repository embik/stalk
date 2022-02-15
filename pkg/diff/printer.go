package diff

import (
	"fmt"
	"time"

	"github.com/gookit/color"
	"github.com/shibukawa/cdiff"
	"github.com/sirupsen/logrus"
	"go.xrstf.de/stalk/pkg/maputil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"
)

type Differ struct {
	opt *Options
	log logrus.FieldLogger
}

func NewDiffer(opt *Options, log logrus.FieldLogger) (*Differ, error) {
	if err := opt.Validate(); err != nil {
		return nil, fmt.Errorf("invalid options: %w", err)
	}

	return &Differ{
		opt: opt,
		log: log,
	}, nil
}

func (d *Differ) PrintDiff(oldObj, newObj *unstructured.Unstructured, lastSeen time.Time) error {
	oldString, err := d.preprocess(oldObj)
	if err != nil {
		return fmt.Errorf("failed to process previous object: %w", err)
	}

	newString, err := d.preprocess(newObj)
	if err != nil {
		return fmt.Errorf("failed to process current object: %w", err)
	}

	titleA := diffTitle(oldObj, lastSeen)
	titleB := diffTitle(newObj, time.Now())

	colorTheme := d.opt.UpdateColorTheme
	if oldObj == nil {
		colorTheme = d.opt.CreateColorTheme
	}
	if newObj == nil {
		colorTheme = d.opt.DeleteColorTheme
	}

	diff := cdiff.Diff(oldString, newString, cdiff.WordByWord)
	color.Print(diff.UnifiedWithGooKitColor(titleA, titleB, d.opt.ContextLines, colorTheme))

	return nil
}

func (d *Differ) preprocess(obj *unstructured.Unstructured) (string, error) {
	if obj == nil {
		return "", nil
	}

	generic, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("failed to encode object as JSON: %w", err)
	}

	var genericObj map[string]interface{}
	if err := json.Unmarshal(generic, &genericObj); err != nil {
		return "", fmt.Errorf("failed to re-decode object from JSON: %w", err)
	}

	if d.opt.compiledJSONPath != nil {
		results, err := d.opt.compiledJSONPath.FindResults(genericObj)
		if err != nil {
			d.log.Warnf("Failed to apply JSON path: %w", err)
		} else if len(results) > 0 && len(results[0]) > 0 {
			generic, err = json.Marshal(results[0][0].Interface())
			if err != nil {
				return "", fmt.Errorf("failed to encode JSON path result as JSON: %w", err)
			}

			if err := json.Unmarshal(generic, &genericObj); err != nil {
				return "", fmt.Errorf("failed to re-decode JSON path result from JSON: %w", err)
			}
		}
	}

	// for _, includePath := range d.opt.parsedIncludePaths {

	// }

	if len(d.opt.parsedExcludePaths) > 0 {
		for _, excludePath := range d.opt.parsedExcludePaths {
			genericObj, err = maputil.RemovePath(genericObj, excludePath)
			if err != nil {
				return "", fmt.Errorf("failed to apply exclude expression %v: %w", excludePath, err)
			}
		}

		generic, err = json.Marshal(genericObj)
		if err != nil {
			return "", fmt.Errorf("failed to encode exclude expression result as JSON: %w", err)
		}
	}

	final, err := yaml.JSONToYAML(generic)
	if err != nil {
		return "", fmt.Errorf("failed to encode object as YAML: %w", err)
	}

	return string(final), nil
}

func objectKey(obj *unstructured.Unstructured) string {
	key := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		key = fmt.Sprintf("%s/%s", ns, key)
	}

	return key
}

func diffTitle(obj *unstructured.Unstructured, lastSeen time.Time) string {
	if obj == nil {
		return "(none)"
	}

	timestamp := lastSeen.Format(time.RFC3339)
	kind := obj.GroupVersionKind().Kind

	return fmt.Sprintf("%s %s v%s (%s) (gen. %d)", kind, objectKey(obj), timestamp, obj.GetResourceVersion(), obj.GetGeneration())
}

func yamlEncode(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}

	encoded, _ := yaml.Marshal(obj)

	return string(encoded)
}