package detect

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/asmcos/requests"
	"github.com/google/go-github/github"
	"github.com/pingcap/ci/sync_ci/pkg/db"
	"github.com/pingcap/ci/sync_ci/pkg/model"
	"github.com/pingcap/ci/sync_ci/pkg/parser"
	"github.com/pingcap/log"
	"gorm.io/gorm"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const NEW_CASE = 0
const RETRIGGERED_CASE = 1
const searchIssueIntervalStr = "178h"
const PrInspectLimit = time.Hour * 24 * 7
const baselink = "https://internal.pingcap.net/idc-jenkins/job/%s/%s/display/redirect" // job_name, job_id
type repoPrCases map[string]map[string][]string
type repoCasePrSet map[string]map[string]map[string]bool

const FirstCaseOnly = true

func GetCasesFromPR(cfg model.Config, startTime time.Time, inspectStartTime time.Time, test bool) ([]*model.CaseIssue, error) {
	cidb := db.DBWarehouse[db.CIDBName]

	// Get failed cases from CI data
	now := time.Now()

	rows, err := cidb.Raw(model.GetCICaseSql, formatT(inspectStartTime), formatT(startTime)).Rows()
	if err != nil {
		return nil, err
	}
	caseSet := repoPrCases{}
	_ = getHistoryCases(rows, caseSet, baselink)

	recentRows, err := cidb.Raw(model.GetCICaseSql, formatT(startTime), formatT(now)).Rows()
	if err != nil {
		return nil, err
	}
	DupRecentCaseSet := map[string]map[string][]string{}
	allRecentCases := getDuplicatesFromHistory(recentRows, caseSet, DupRecentCaseSet)

	dupCaseStr, err := json.Marshal(DupRecentCaseSet)
	log.S().Info("Acquired duplicate cases: ", string(dupCaseStr))
	log.S().Info("Filtering cases to create issue with")

	// Validate repo cases
	dbIssueCase := db.DBWarehouse[db.CIDBName]
	dbGithub := db.DBWarehouse[db.GithubDBName]
	// create alerts and filter requiredCases
	rowsToRemindPr, err := cidb.Raw(model.GetCICaseSql, formatT(startTime), formatT(now)).Rows()
	if err != nil {
		return nil, err
	}
	casesToRemind := getHistoryCases(rowsToRemindPr, repoPrCases{}, baselink)
	handlePrReminder(cfg, dbGithub, dbIssueCase, casesToRemind, test)

	// assumed `cases` param has no reps
	_, err = handleCasesIfIssueExists(cfg, allRecentCases, dbIssueCase, dbGithub, true, test)
	issuesToCreate, err := handleCasesIfIssueExists(cfg, DupRecentCaseSet, dbIssueCase, dbGithub, false, test)
	if err != nil {
		return nil, err
	}

	_ = rows.Close()
	_ = recentRows.Close()
	return issuesToCreate, nil
}

func handlePrReminder(cfg model.Config, dbGithub *gorm.DB, dbIssueCase *gorm.DB, recentCases repoCasePrSet, test bool){
	for r, casePrs := range recentCases {
		for c, prSet := range casePrs{
			// Check if issue exists and fetch latest
			existedCases, err := dbIssueCase.Raw(model.IssueCaseExistsSql, c, r).Rows()
			if err != nil {
				log.S().Error("failed to check existing [case, repo]: ", c, r)
				continue
			}
			if !existedCases.Next() {
				// no issue found. do nothing
				continue
			} else {
				// check time
				var issueNumStr string
				err = existedCases.Scan(&issueNumStr)
				issueNumberLike := "%/" + issueNumStr
				repoLike := "%/" + r + "/%"
				stillValidIssues, err := dbGithub.Raw(model.CheckClosedIssue, issueNumberLike, repoLike, searchIssueIntervalStr).Rows()
				if err != nil {
					log.S().Error("failed to check existing [case, repo]: ", c, r)
					continue
				}
				if !stillValidIssues.Next() {
					continue
				}

				issueLink := fmt.Sprintf("https://www.github.com/repos/%s/issues/%s", r, issueNumStr)
				for pr, _ := range prSet {
					err = RemindMergePr(cfg, r, pr, c, issueLink, test)
					if err != nil {
						log.S().Error("failed to remind merge pr: ", c, r)
						continue
					}
				}
			}
		}
	}
}

func handleCasesIfIssueExists(cfg model.Config, recentCaseSet repoPrCases, dbIssueCase *gorm.DB, dbGithub *gorm.DB, mentionExisted, test bool) ([]*model.CaseIssue, error) {
	issueCases := []*model.CaseIssue{}
	for repo, repoCases := range recentCaseSet {
		for c, v := range repoCases {
			existedCases, err := dbIssueCase.Raw(model.IssueCaseExistsSql, c, repo).Rows()
			if err != nil {
				log.S().Error("failed to check existing [case, repo]: ", c, repo)
				continue
			}
			if !existedCases.Next() {
				issueCase := model.CaseIssue{
					IssueNo:   NEW_CASE,
					Repo:      repo,
					IssueLink: sql.NullString{},
					Case:      sql.NullString{c, true},
					JobLink:   sql.NullString{v[0], true},
				}
				issueCases = append(issueCases, &issueCase)
				if err = RemindUnloggedCasePr(cfg, repo, "", c, test); err != nil {
					log.S().Error("Remind unlogged case failed for ", c, ", with job link ", v[0])
				}else{
					log.S().Info("Unlogged case reminded")
				}
			} else {
				var issueNumStr string
				err = existedCases.Scan(&issueNumStr)
				if err != nil {
					log.S().Error("failed to obtain issue num", err)
					continue
				}

				issueCases, err = handleCaseIfHistoryExists(cfg, dbGithub, issueNumStr, repo, c, v, issueCases, mentionExisted, test)
				if err != nil {
					log.S().Error("failed to respond to issueCases", err)
					continue
				}
			}
		}
	}
	return issueCases, nil
}

func handleCaseIfHistoryExists(cfg model.Config, dbGithub *gorm.DB, issueNumStr string, repo string, caseName string, joblinks []string, issueCases []*model.CaseIssue, mentionExisted, test bool) ([]*model.CaseIssue, error) {
			issueNumberLike := "%/" + issueNumStr
			repoLike := "%/" + repo + "/%"
			stillValidIssues, err := dbGithub.Raw(model.IssueRecentlyOpenSql, issueNumberLike, repoLike, searchIssueIntervalStr).Rows()
			if err != nil {
				return nil, err
			}
			closedIssues, err := dbGithub.Raw(model.IssueClosed, issueNumberLike, repoLike, searchIssueIntervalStr).Rows()
			if err != nil {
				return nil, err
			}

			if !stillValidIssues.Next() {
				if closedIssues.Next() {  // the issue was closed
					issueCase := model.CaseIssue{
						IssueNo:   RETRIGGERED_CASE,
						Repo:      repo,
						IssueLink: sql.NullString{},
						Case:      sql.NullString{caseName, true},
						JobLink:   sql.NullString{joblinks[0], true},
					}
					issueCases = append(issueCases, &issueCase)
				}else{
					issueCase := model.CaseIssue{
						IssueNo:   NEW_CASE,
						Repo:      repo,
						IssueLink: sql.NullString{},
						Case:      sql.NullString{caseName, true},
						JobLink:   sql.NullString{joblinks[0], true},
					}
					issueCases = append(issueCases, &issueCase)
				}
			} else if mentionExisted { // mention existing issue
				var url string
				err = stillValidIssues.Scan(&url)
		if err != nil {
			log.S().Error("failed to extract existing issue url", err)
			return nil, err
		}
		log.S().Info("Mentioning issue located at ", url)
		issueId := strings.Split(url, "/issues/")[1]
		err = MentionIssue(cfg, repo, issueId, joblinks[0], test)
		if err != nil {
			log.S().Error(err)
			return nil, err
		}
	}
	return issueCases, nil
}

func GetNightlyCases(cfg model.Config, filterStartTime, now time.Time, test bool) ([]*model.CaseIssue, error) {
	cidb := db.DBWarehouse[db.CIDBName]
	ghdb := db.DBWarehouse[db.GithubDBName]
	csdb := db.DBWarehouse[db.CIDBName]
	rows, err := cidb.Raw(model.GetCINightlyCase, formatT(filterStartTime), formatT(now)).Rows()
	RepoNightlyCase := map[string]map[string][]string{}
	issueCases := []*model.CaseIssue{}

	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rawCase []byte
		var cases []string
		var jobid string
		var job string
		err = rows.Scan(&rawCase, &jobid, &job)
		if err != nil {
			log.S().Error(err)
			continue
		}
		err = json.Unmarshal(rawCase, &cases)
		if err != nil {
			log.S().Error(err)
			continue
		}
		repo := extractRepoFromJobName(job)
		if repo == "others" { // mixed unknown repos
			continue
		}

		for _, c := range cases {
			if _, ok := RepoNightlyCase[repo]; !ok {
				RepoNightlyCase[repo] = map[string][]string{}
			}
			if _, ok := RepoNightlyCase[repo][c]; !ok {
				link := fmt.Sprintf(baselink, job, jobid)
				RepoNightlyCase[repo][c] = []string{link}

				issueCase := model.CaseIssue{
					IssueNo:   0,
					Repo:      repo,
					IssueLink: sql.NullString{},
					Case:      sql.NullString{c, true},
					JobLink:   sql.NullString{link, true},
				}
				issueCases = append(issueCases, &issueCase)
			}
			if FirstCaseOnly {
				break
			}
		}
	}
	issueCases, err = handleCasesIfIssueExists(cfg, RepoNightlyCase, csdb, ghdb, true, test)
	if err != nil {
		log.S().Error("Failed to handle existing issue case", err)
	}

	return issueCases, nil
}

func extractRepoFromJobName(job string) string {
	tidb_regex := regexp.MustCompile("^tidb_ghpr")
	tikv_regex := regexp.MustCompile("^tikv_ghpr")
	pd_regex := regexp.MustCompile("^pd_ghpr")
	if tidb_regex.MatchString(job) {
		return "pingcap/tidb"
	}
	if tikv_regex.MatchString(job) {
		return "tikv/tikv"
	}
	if pd_regex.MatchString(job) {
		return "tikv/pd"
	}
	return "others"
}

func getDuplicatesFromHistory(recentRows *sql.Rows, caseSet map[string]map[string][]string, recentCaseSet map[string]map[string][]string) map[string]map[string][]string {
	allRecentCases := map[string]map[string][]string{}
	for recentRows.Next() {
		var rawCase []byte
		var cases []string
		var pr string
		var repo string
		var jobid string
		var job string
		err := recentRows.Scan(&repo, &pr, &rawCase, &jobid, &job)
		if err != nil {
			log.S().Error(err)
			continue
		}
		err = json.Unmarshal(rawCase, &cases)
		if err != nil {
			log.S().Error(err)
			continue
		}

		for _, c := range cases {
			if _, ok := allRecentCases[repo]; !ok {
				allRecentCases[repo] = map[string][]string{}
			}
			if _, ok := allRecentCases[repo][c]; !ok {
				allRecentCases[repo][c] = []string{}
			}
			jobLink := fmt.Sprintf(baselink, job, jobid)
			allRecentCases[repo][c] = append(allRecentCases[repo][c], jobLink)
			if _, ok := caseSet[repo]; !ok {
				caseSet[repo] = map[string][]string{}
			}
			if _, ok := caseSet[repo][c]; ok {
				if _, ok := recentCaseSet[repo]; !ok {
					recentCaseSet[repo] = map[string][]string{}
				}
				if matched, name := parser.MatchAndParseSQLStmtTest(c); matched {
					recentCaseSet[repo][name] = append([]string{jobLink}, caseSet[repo][c]...)
				} else {
					recentCaseSet[repo][c] = append([]string{jobLink}, caseSet[repo][c]...)
				}
			}
			if FirstCaseOnly {
				break
			}
		}
	}
	return allRecentCases
}

func getHistoryCases(rows *sql.Rows, caseSet map[string]map[string][]string, baselink string) map[string]map[string]map[string]bool {
	repoPrCases := map[string]map[string]map[string]bool{} // repo -> pr -> case
	for rows.Next() {
		var rawCase []byte
		var cases []string
		var pr string
		var repo string
		var jobid string
		var job string
		err := rows.Scan(&repo, &pr, &rawCase, &jobid, &job)
		if err != nil {
			log.S().Error("error getting history", err)
			continue
		}
		err = json.Unmarshal(rawCase, &cases)
		if err != nil {
			log.S().Error("error getting history", err)
			continue
		}
		for _, c := range cases {
			if _, ok := repoPrCases[repo]; !ok {
				repoPrCases[repo] = map[string]map[string]bool{}
			}
			if _, ok := repoPrCases[repo][c]; !ok {
				repoPrCases[repo][c] = map[string]bool{pr: true}
			} else {
				repoPrCases[repo][c][pr] = true
				if _, ok = caseSet[repo]; !ok {
					caseSet[repo] = map[string][]string{}
				}
				caseSet[repo][c] = append(caseSet[repo][c], fmt.Sprintf(baselink, job, jobid))
			}
			if FirstCaseOnly {
				break
			}
		}
	}
	return repoPrCases
}

func MentionIssue(cfg model.Config, repo string, issueId string, joblink string, test bool) error {
	req := requests.Requests()
	req.SetTimeout(10 * time.Second)
	req.Header.Set("Authorization", "token "+cfg.GithubToken)
	baseComment := `Yet another case failure: <a href="%s">%s</a>`
	var url string
	if !test {
		url = fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments", repo, issueId)
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/issues/%s/comments", repo, issueId)
	}

	for i := 0; i < 3; i++ {
		log.S().Info("Posting to ", url)
		resp, err := req.PostJson(url, map[string]string{
			"body": fmt.Sprintf(baseComment, joblink, joblink),
		})
		if err != nil {
			log.S().Error("Error commenting issue '", url, "'; Error: ", err, "; Retry")
		} else {
			if resp.R.StatusCode != 201 {
				log.S().Error("Error commenting issue ", url, ". Retry")
			} else {
				log.S().Infof("Created comment %s/#%s mentioning %s", repo, issueId, joblink)
				return nil
			}
		}
	}
	return fmt.Errorf("failed to mention existing issue at %s for job at %s", url, joblink)
}

func CreateIssueForCases(cfg model.Config, issues []*model.CaseIssue, test bool) error {
	req := requests.Requests()
	req.SetTimeout(10 * time.Second)
	req.Header.Set("Authorization", "token "+cfg.GithubToken)
	dbIssueCase := db.DBWarehouse[db.CIDBName]
	// todo: set request header
	for _, issue := range issues {
		var url string
		if !test {
			url = fmt.Sprintf("https://api.github.com/repos/%s/issues", issue.Repo)
		} else {
			url = "https://api.github.com/repos/kivenchen/klego/issues"
		}
		var resp *requests.Response
		for i := 0; i < 3; i++ {
			log.S().Info("Posting to ", url)

			var title string
			if issue.IssueNo == NEW_CASE{
				title = issue.Case.String + " failed"
			}else if issue.IssueNo == RETRIGGERED_CASE {
				title = "Resolved unstable case failure: " + issue.Case.String
			}

			resp, err := req.PostJson(url, map[string]interface{}{
				"title":  title,
				"body":   "Latest build: <a href=\"" + issue.JobLink.String + "\">" + issue.JobLink.String + "</a>", // todo: fill content templates
				"labels": []string{"component/test"},
			})
			if err != nil {
				log.S().Error("Error creating issue '", url, "'; Error: ", err, "; Retry")
			} else {
				if resp.R.StatusCode != 201 {
					log.S().Error("Error creating issue ", url, ". Retry")
					// might be nil
					//log.S().Error("Create issue failed: ", string(resp.Content()))
				} else {
					log.S().Info("create issue success for job", issue.JobLink.String)
					break
				}
			}
		}

		if resp == nil || resp.R.StatusCode != 201 {
			log.S().Error("Error commenting issue ", url, ". Skipped")
			log.S().Error("Create issue failed: ", resp.R.StatusCode, string(resp.Content()))
			continue
		}

		responseDict := github.Issue{}
		err := resp.Json(&responseDict)
		if err != nil {
			log.S().Error("parse response failed", err)
			continue
		}

		num := responseDict.Number
		link := reflect.ValueOf(responseDict.URL).Elem().String()
		issue.IssueNo = reflect.ValueOf(num).Elem().Int()
		issue.IssueLink = sql.NullString{
			String: link,
			Valid:  true,
		}
		//log db
		dbIssueCase.Create(issue)
		if dbIssueCase.Error != nil {
			log.S().Error("Log issue_case db failed", dbIssueCase.Error)
		}
		dbIssueCase.Commit()
		if dbIssueCase.Error != nil {
			log.S().Error("Log issue_case db commit failed", dbIssueCase.Error)
		}
	}
	return nil
}

func formatT(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}
