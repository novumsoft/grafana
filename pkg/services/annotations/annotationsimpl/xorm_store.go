package annotationsimpl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/annotations"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/services/sqlstore/db"
	"github.com/grafana/grafana/pkg/services/sqlstore/permissions"
	"github.com/grafana/grafana/pkg/services/sqlstore/searchstore"
	"github.com/grafana/grafana/pkg/services/tag"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/setting"
)

var timeNow = time.Now

// Update the item so that EpochEnd >= Epoch
func validateTimeRange(item *annotations.Item) error {
	if item.EpochEnd == 0 {
		if item.Epoch == 0 {
			return annotations.ErrTimerangeMissing
		}
		item.EpochEnd = item.Epoch
	}
	if item.Epoch == 0 {
		item.Epoch = item.EpochEnd
	}
	if item.EpochEnd < item.Epoch {
		item.Epoch, item.EpochEnd = item.EpochEnd, item.Epoch
	}
	return nil
}

type xormRepositoryImpl struct {
	cfg        *setting.Cfg
	db         db.DB
	log        log.Logger
	tagService tag.Service
}

func (r *xormRepositoryImpl) Add(ctx context.Context, item *annotations.Item) error {
	tags := tag.ParseTagPairs(item.Tags)
	item.Tags = tag.JoinTagPairs(tags)
	item.Created = timeNow().UnixNano() / int64(time.Millisecond)
	item.Updated = item.Created
	if item.Epoch == 0 {
		item.Epoch = item.Created
	}
	if err := validateTimeRange(item); err != nil {
		return err
	}

	return r.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		if _, err := sess.Table("annotation").Insert(item); err != nil {
			return err
		}

		if item.Tags != nil {
			tags, err := r.tagService.EnsureTagsExist(ctx, tags)
			if err != nil {
				return err
			}
			for _, tag := range tags {
				if _, err := sess.Exec("INSERT INTO annotation_tag (annotation_id, tag_id) VALUES(?,?)", item.Id, tag.Id); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (r *xormRepositoryImpl) Update(ctx context.Context, item *annotations.Item) error {
	return r.db.WithTransactionalDbSession(ctx, func(sess *sqlstore.DBSession) error {
		var (
			isExist bool
			err     error
		)
		existing := new(annotations.Item)

		isExist, err = sess.Table("annotation").Where("id=? AND org_id=?", item.Id, item.OrgId).Get(existing)

		if err != nil {
			return err
		}
		if !isExist {
			return errors.New("annotation not found")
		}

		existing.Updated = timeNow().UnixNano() / int64(time.Millisecond)
		existing.Text = item.Text

		if item.Epoch != 0 {
			existing.Epoch = item.Epoch
		}
		if item.EpochEnd != 0 {
			existing.EpochEnd = item.EpochEnd
		}

		if err := validateTimeRange(existing); err != nil {
			return err
		}

		if item.Tags != nil {
			tags, err := r.tagService.EnsureTagsExist(ctx, tag.ParseTagPairs(item.Tags))
			if err != nil {
				return err
			}
			if _, err := sess.Exec("DELETE FROM annotation_tag WHERE annotation_id = ?", existing.Id); err != nil {
				return err
			}
			for _, tag := range tags {
				if _, err := sess.Exec("INSERT INTO annotation_tag (annotation_id, tag_id) VALUES(?,?)", existing.Id, tag.Id); err != nil {
					return err
				}
			}
		}

		existing.Tags = item.Tags

		_, err = sess.Table("annotation").ID(existing.Id).Cols("epoch", "text", "epoch_end", "updated", "tags").Update(existing)
		return err
	})
}

func (r *xormRepositoryImpl) Get(ctx context.Context, query *annotations.ItemQuery) ([]*annotations.ItemDTO, error) {
	var sql bytes.Buffer
	params := make([]interface{}, 0)
	items := make([]*annotations.ItemDTO, 0)
	err := r.db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		sql.WriteString(`
			SELECT
				annotation.id,
				annotation.epoch as time,
				annotation.epoch_end as time_end,
				annotation.dashboard_id,
				annotation.panel_id,
				annotation.new_state,
				annotation.prev_state,
				annotation.alert_id,
				annotation.text,
				annotation.tags,
				annotation.data,
				annotation.created,
				annotation.updated,
				usr.email,
				usr.login,
				alert.name as alert_name
			FROM annotation
			LEFT OUTER JOIN ` + r.db.GetDialect().Quote("user") + ` as usr on usr.id = annotation.user_id
			LEFT OUTER JOIN alert on alert.id = annotation.alert_id
			INNER JOIN (
				SELECT a.id from annotation a
			`)

		sql.WriteString(`WHERE a.org_id = ?`)
		params = append(params, query.OrgId)

		if query.AnnotationId != 0 {
			// fmt.Print("annotation query")
			sql.WriteString(` AND a.id = ?`)
			params = append(params, query.AnnotationId)
		}

		if query.AlertId != 0 {
			sql.WriteString(` AND a.alert_id = ?`)
			params = append(params, query.AlertId)
		}

		if query.DashboardId != 0 {
			sql.WriteString(` AND a.dashboard_id = ?`)
			params = append(params, query.DashboardId)
		}

		if query.PanelId != 0 {
			sql.WriteString(` AND a.panel_id = ?`)
			params = append(params, query.PanelId)
		}

		if query.UserId != 0 {
			sql.WriteString(` AND a.user_id = ?`)
			params = append(params, query.UserId)
		}

		if query.From > 0 && query.To > 0 {
			sql.WriteString(` AND a.epoch <= ? AND a.epoch_end >= ?`)
			params = append(params, query.To, query.From)
		}

		if query.Type == "alert" {
			sql.WriteString(` AND a.alert_id > 0`)
		} else if query.Type == "annotation" {
			sql.WriteString(` AND a.alert_id = 0`)
		}

		if len(query.Tags) > 0 {
			keyValueFilters := []string{}

			tags := tag.ParseTagPairs(query.Tags)
			for _, tag := range tags {
				if tag.Value == "" {
					keyValueFilters = append(keyValueFilters, "(tag."+r.db.GetDialect().Quote("key")+" = ?)")
					params = append(params, tag.Key)
				} else {
					keyValueFilters = append(keyValueFilters, "(tag."+r.db.GetDialect().Quote("key")+" = ? AND tag."+r.db.GetDialect().Quote("value")+" = ?)")
					params = append(params, tag.Key, tag.Value)
				}
			}

			if len(tags) > 0 {
				tagsSubQuery := fmt.Sprintf(`
			SELECT SUM(1) FROM annotation_tag at
			INNER JOIN tag on tag.id = at.tag_id
			WHERE at.annotation_id = a.id
				AND (
				%s
				)
		`, strings.Join(keyValueFilters, " OR "))

				if query.MatchAny {
					sql.WriteString(fmt.Sprintf(" AND (%s) > 0 ", tagsSubQuery))
				} else {
					sql.WriteString(fmt.Sprintf(" AND (%s) = %d ", tagsSubQuery, len(tags)))
				}
			}
		}

		if !ac.IsDisabled(r.cfg) {
			acFilter, acArgs, err := getAccessControlFilter(query.SignedInUser)
			if err != nil {
				return err
			}
			sql.WriteString(fmt.Sprintf(" AND (%s)", acFilter))
			params = append(params, acArgs...)
		}

		if query.Limit == 0 {
			query.Limit = 100
		}

		// order of ORDER BY arguments match the order of a sql index for performance
		sql.WriteString(" ORDER BY a.org_id, a.epoch_end DESC, a.epoch DESC" + r.db.GetDialect().Limit(query.Limit) + " ) dt on dt.id = annotation.id")
		if err := sess.SQL(sql.String(), params...).Find(&items); err != nil {
			items = nil
			return err
		}
		return nil
	},
	)

	return items, err
}

func getAccessControlFilter(user *user.SignedInUser) (string, []interface{}, error) {
	if user == nil || user.Permissions[user.OrgID] == nil {
		return "", nil, errors.New("missing permissions")
	}
	scopes, has := user.Permissions[user.OrgID][ac.ActionAnnotationsRead]
	if !has {
		return "", nil, errors.New("missing permissions")
	}
	types, hasWildcardScope := ac.ParseScopes(ac.ScopeAnnotationsProvider.GetResourceScopeType(""), scopes)
	if hasWildcardScope {
		types = map[interface{}]struct{}{annotations.Dashboard.String(): {}, annotations.Organization.String(): {}}
	}

	var filters []string
	var params []interface{}
	for t := range types {
		// annotation read permission with scope annotations:type:organization allows listing annotations that are not associated with a dashboard
		if t == annotations.Organization.String() {
			filters = append(filters, "a.dashboard_id = 0")
		}
		// annotation read permission with scope annotations:type:dashboard allows listing annotations from dashboards which the user can view
		if t == annotations.Dashboard.String() {
			dashboardFilter, dashboardParams := permissions.NewAccessControlDashboardPermissionFilter(user, models.PERMISSION_VIEW, searchstore.TypeDashboard).Where()
			filter := fmt.Sprintf("a.dashboard_id IN(SELECT id FROM dashboard WHERE %s)", dashboardFilter)
			filters = append(filters, filter)
			params = dashboardParams
		}
	}
	return strings.Join(filters, " OR "), params, nil
}

func (r *xormRepositoryImpl) Delete(ctx context.Context, params *annotations.DeleteParams) error {
	return r.db.WithTransactionalDbSession(ctx, func(sess *sqlstore.DBSession) error {
		var (
			sql        string
			annoTagSQL string
		)

		r.log.Info("delete", "orgId", params.OrgId)
		if params.Id != 0 {
			annoTagSQL = "DELETE FROM annotation_tag WHERE annotation_id IN (SELECT id FROM annotation WHERE id = ? AND org_id = ?)"
			sql = "DELETE FROM annotation WHERE id = ? AND org_id = ?"

			if _, err := sess.Exec(annoTagSQL, params.Id, params.OrgId); err != nil {
				return err
			}

			if _, err := sess.Exec(sql, params.Id, params.OrgId); err != nil {
				return err
			}
		} else {
			annoTagSQL = "DELETE FROM annotation_tag WHERE annotation_id IN (SELECT id FROM annotation WHERE dashboard_id = ? AND panel_id = ? AND org_id = ?)"
			sql = "DELETE FROM annotation WHERE dashboard_id = ? AND panel_id = ? AND org_id = ?"

			if _, err := sess.Exec(annoTagSQL, params.DashboardId, params.PanelId, params.OrgId); err != nil {
				return err
			}

			if _, err := sess.Exec(sql, params.DashboardId, params.PanelId, params.OrgId); err != nil {
				return err
			}
		}

		return nil
	})
}

func (r *xormRepositoryImpl) GetTags(ctx context.Context, query *annotations.TagsQuery) (annotations.FindTagsResult, error) {
	var items []*annotations.Tag
	err := r.db.WithDbSession(ctx, func(dbSession *sqlstore.DBSession) error {
		if query.Limit == 0 {
			query.Limit = 100
		}

		var sql bytes.Buffer
		params := make([]interface{}, 0)
		tagKey := `tag.` + r.db.GetDialect().Quote("key")
		tagValue := `tag.` + r.db.GetDialect().Quote("value")

		sql.WriteString(`
		SELECT
			` + tagKey + `,
			` + tagValue + `,
			count(*) as count
		FROM tag
		INNER JOIN annotation_tag ON tag.id = annotation_tag.tag_id
`)

		sql.WriteString(`WHERE EXISTS(SELECT 1 FROM annotation WHERE annotation.id = annotation_tag.annotation_id AND annotation.org_id = ?)`)
		params = append(params, query.OrgID)

		sql.WriteString(` AND (` + tagKey + ` ` + r.db.GetDialect().LikeStr() + ` ? OR ` + tagValue + ` ` + r.db.GetDialect().LikeStr() + ` ?)`)
		params = append(params, `%`+query.Tag+`%`, `%`+query.Tag+`%`)

		sql.WriteString(` GROUP BY ` + tagKey + `,` + tagValue)
		sql.WriteString(` ORDER BY ` + tagKey + `,` + tagValue)
		sql.WriteString(` ` + r.db.GetDialect().Limit(query.Limit))

		err := dbSession.SQL(sql.String(), params...).Find(&items)
		return err
	})
	if err != nil {
		return annotations.FindTagsResult{Tags: []*annotations.TagsDTO{}}, err
	}
	tags := make([]*annotations.TagsDTO, 0)
	for _, item := range items {
		tag := item.Key
		if len(item.Value) > 0 {
			tag = item.Key + ":" + item.Value
		}
		tags = append(tags, &annotations.TagsDTO{
			Tag:   tag,
			Count: item.Count,
		})
	}

	return annotations.FindTagsResult{Tags: tags}, nil
}

func (r *xormRepositoryImpl) CleanAnnotations(ctx context.Context, cfg setting.AnnotationCleanupSettings, annotationType string) (int64, error) {
	var totalAffected int64
	if cfg.MaxAge > 0 {
		cutoffDate := time.Now().Add(-cfg.MaxAge).UnixNano() / int64(time.Millisecond)
		deleteQuery := `DELETE FROM annotation WHERE id IN (SELECT id FROM (SELECT id FROM annotation WHERE %s AND created < %v ORDER BY id DESC %s) a)`
		sql := fmt.Sprintf(deleteQuery, annotationType, cutoffDate, r.db.GetDialect().Limit(r.cfg.AnnotationCleanupJobBatchSize))

		affected, err := r.executeUntilDoneOrCancelled(ctx, sql)
		totalAffected += affected
		if err != nil {
			return totalAffected, err
		}
	}

	if cfg.MaxCount > 0 {
		deleteQuery := `DELETE FROM annotation WHERE id IN (SELECT id FROM (SELECT id FROM annotation WHERE %s ORDER BY id DESC %s) a)`
		sql := fmt.Sprintf(deleteQuery, annotationType, r.db.GetDialect().LimitOffset(r.cfg.AnnotationCleanupJobBatchSize, cfg.MaxCount))
		affected, err := r.executeUntilDoneOrCancelled(ctx, sql)
		totalAffected += affected
		return totalAffected, err
	}

	return totalAffected, nil
}

func (r *xormRepositoryImpl) CleanOrphanedAnnotationTags(ctx context.Context) (int64, error) {
	deleteQuery := `DELETE FROM annotation_tag WHERE id IN ( SELECT id FROM (SELECT id FROM annotation_tag WHERE NOT EXISTS (SELECT 1 FROM annotation a WHERE annotation_id = a.id) %s) a)`
	sql := fmt.Sprintf(deleteQuery, r.db.GetDialect().Limit(r.cfg.AnnotationCleanupJobBatchSize))
	return r.executeUntilDoneOrCancelled(ctx, sql)
}

func (r *xormRepositoryImpl) executeUntilDoneOrCancelled(ctx context.Context, sql string) (int64, error) {
	var totalAffected int64
	for {
		select {
		case <-ctx.Done():
			return totalAffected, ctx.Err()
		default:
			var affected int64
			err := r.db.WithDbSession(ctx, func(session *sqlstore.DBSession) error {
				res, err := session.Exec(sql)
				if err != nil {
					return err
				}

				affected, err = res.RowsAffected()
				totalAffected += affected

				return err
			})
			if err != nil {
				return totalAffected, err
			}

			if affected == 0 {
				return totalAffected, nil
			}
		}
	}
}
